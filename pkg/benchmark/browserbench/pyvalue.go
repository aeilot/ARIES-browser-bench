package browserbench

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The AssistantBench scorer is Python whose behavior depends on dynamic
// types: json.loads may turn an answer into an int, a dict or a list, and
// later branches test type(...) and call str(...) on what they got. pyValue
// models just the Python values that scorer can see, so the Go port follows
// the same branches instead of approximating them.
type pyKind int

const (
	pyNone pyKind = iota
	pyBool
	pyInt
	pyFloat
	pyStr
	pyList
	pyTuple
	pyDict
)

type pyValue struct {
	kind  pyKind
	b     bool
	i     int64
	f     float64
	s     string
	items []pyValue // list and tuple elements
	keys  []string  // dict keys in insertion order
	dict  map[string]pyValue
}

func pyString(value string) pyValue  { return pyValue{kind: pyStr, s: value} }
func pyNumber(value float64) pyValue { return pyValue{kind: pyFloat, f: value} }

// hasLen reports whether Python's len() accepts the value.
func (v pyValue) hasLen() bool {
	switch v.kind {
	case pyStr, pyList, pyTuple, pyDict:
		return true
	}
	return false
}

func (v pyValue) length() int {
	switch v.kind {
	case pyStr:
		return utf8.RuneCountInString(v.s)
	case pyList, pyTuple:
		return len(v.items)
	case pyDict:
		return len(v.keys)
	}
	return 0
}

func (v pyValue) typeName() string {
	return [...]string{"NoneType", "bool", "int", "float", "str", "list", "tuple", "dict"}[v.kind]
}

// errPyRaised marks a path on which the upstream Python scorer raises instead
// of returning a score.
var errPyRaised = errors.New("upstream AssistantBench scorer raises")

func pyRaise(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errPyRaised, fmt.Sprintf(format, args...))
}

// pyFloatLiteral is the grammar float() accepts after stripping whitespace
// and mapping Unicode decimal digits to ASCII: digits with single
// underscores between them, an optional fraction and exponent, or
// inf/infinity/nan in any case.
var pyFloatLiteral = regexp.MustCompile(`^[+-]?(?:(?:\d(?:_?\d)*\.(?:\d(?:_?\d)*)?|\.\d(?:_?\d)*|\d(?:_?\d)*)(?:[eE][+-]?\d(?:_?\d)*)?|(?i:inf|infinity|nan))$`)

// asciiDigits maps every Unicode decimal digit to its ASCII digit. Unicode
// encodes each script's decimal digits as consecutive runs from zero to nine.
func asciiDigits(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x80 || !unicode.Is(unicode.Nd, r) {
			return r
		}
		first := r
		for unicode.Is(unicode.Nd, first-1) {
			first--
		}
		return '0' + (r-first)%10
	}, text)
}

// pyParseFloat is Python's float(str).
func pyParseFloat(text string) (float64, bool) {
	text = asciiDigits(pyStrip(text))
	if !pyFloatLiteral.MatchString(text) {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return value, true
}

// pyIsSpace is str.isspace() for one character: Unicode White_Space plus the
// four ASCII information separators Python also treats as whitespace.
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

func pyStrip(text string) string { return strings.TrimFunc(text, pyIsSpace) }

// pySplitWhitespace is str.split() with no separator.
func pySplitWhitespace(text string) []string { return strings.FieldsFunc(text, pyIsSpace) }

// pyIsWordChar is the re module's Unicode \w: str.isalnum() or underscore.
func pyIsWordChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// pyIsNumeric is str.isnumeric(): nonempty and every character numeric.
// Python also counts some ideographs with a numeric value (such as 五).
func pyIsNumeric(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if !unicode.IsNumber(r) {
			return false
		}
	}
	return true
}

// pyFloatRepr is repr(float): the shortest round-tripping digits, in fixed
// notation for decimal exponents in [-4, 16) and scientific otherwise.
func pyFloatRepr(value float64) string {
	switch {
	case math.IsNaN(value):
		return "nan"
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	}
	sign := ""
	if math.Signbit(value) {
		sign = "-"
		value = -value
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(exponentText)
	digits := strings.Replace(mantissa, ".", "", 1)
	if value == 0 {
		return sign + "0.0"
	}
	if exponent >= -4 && exponent < 16 {
		point := exponent + 1
		switch {
		case point <= 0:
			return sign + "0." + strings.Repeat("0", -point) + digits
		case point >= len(digits):
			return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
		default:
			return sign + digits[:point] + "." + digits[point:]
		}
	}
	formatted := digits[:1]
	if len(digits) > 1 {
		formatted += "." + digits[1:]
	}
	exponentSign := "+"
	if exponent < 0 {
		exponentSign = "-"
		exponent = -exponent
	}
	return fmt.Sprintf("%s%se%s%02d", sign, formatted, exponentSign, exponent)
}

// pyStrRepr is repr(str).
func pyStrRepr(text string) string {
	quote := byte('\'')
	if strings.ContainsRune(text, '\'') && !strings.ContainsRune(text, '"') {
		quote = '"'
	}
	var builder strings.Builder
	builder.WriteByte(quote)
	for _, r := range text {
		switch {
		case r == rune(quote) || r == '\\':
			builder.WriteByte('\\')
			builder.WriteRune(r)
		case r == '\t':
			builder.WriteString(`\t`)
		case r == '\n':
			builder.WriteString(`\n`)
		case r == '\r':
			builder.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&builder, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			builder.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&builder, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&builder, `\u%04x`, r)
		default:
			fmt.Fprintf(&builder, `\U%08x`, r)
		}
	}
	builder.WriteByte(quote)
	return builder.String()
}

// pyToStr is str(value).
func pyToStr(v pyValue) string {
	if v.kind == pyStr {
		return v.s
	}
	return pyRepr(v)
}

func pyRepr(v pyValue) string {
	switch v.kind {
	case pyNone:
		return "None"
	case pyBool:
		if v.b {
			return "True"
		}
		return "False"
	case pyInt:
		return strconv.FormatInt(v.i, 10)
	case pyFloat:
		return pyFloatRepr(v.f)
	case pyStr:
		return pyStrRepr(v.s)
	case pyList, pyTuple:
		parts := make([]string, len(v.items))
		for index, item := range v.items {
			parts[index] = pyRepr(item)
		}
		if v.kind == pyList {
			return "[" + strings.Join(parts, ", ") + "]"
		}
		if len(parts) == 1 {
			return "(" + parts[0] + ",)"
		}
		return "(" + strings.Join(parts, ", ") + ")"
	default:
		parts := make([]string, len(v.keys))
		for index, key := range v.keys {
			parts[index] = pyStrRepr(key) + ": " + pyRepr(v.dict[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
}

// pyJSONLoads is json.loads(text) with Python's defaults: surrounding
// whitespace, NaN/Infinity literals, integers that stay integers, and dicts
// that keep the last value of a repeated key at its first position.
// Integers outside int64 become floats; Python keeps them exact.
func pyJSONLoads(text string) (pyValue, bool) {
	parser := jsonParser{text: text}
	parser.skipSpace()
	value, ok := parser.value()
	if !ok {
		return pyValue{}, false
	}
	parser.skipSpace()
	return value, parser.pos == len(parser.text)
}

type jsonParser struct {
	text string
	pos  int
}

// skipSpace skips the four whitespace characters JSON allows.
func (p *jsonParser) skipSpace() {
	for p.pos < len(p.text) && strings.IndexByte(" \t\n\r", p.text[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *jsonParser) consume(literal string) bool {
	if strings.HasPrefix(p.text[p.pos:], literal) {
		p.pos += len(literal)
		return true
	}
	return false
}

func (p *jsonParser) value() (pyValue, bool) {
	if p.pos >= len(p.text) {
		return pyValue{}, false
	}
	switch character := p.text[p.pos]; {
	case character == '{':
		return p.object()
	case character == '[':
		return p.array()
	case character == '"':
		text, ok := p.string()
		return pyString(text), ok
	case p.consume("null"):
		return pyValue{kind: pyNone}, true
	case p.consume("true"):
		return pyValue{kind: pyBool, b: true}, true
	case p.consume("false"):
		return pyValue{kind: pyBool}, true
	case p.consume("NaN"):
		return pyNumber(math.NaN()), true
	case p.consume("Infinity"):
		return pyNumber(math.Inf(1)), true
	case p.consume("-Infinity"):
		return pyNumber(math.Inf(-1)), true
	default:
		return p.number()
	}
}

var jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?`)

func (p *jsonParser) number() (pyValue, bool) {
	match := jsonNumber.FindStringSubmatchIndex(p.text[p.pos:])
	if match == nil {
		return pyValue{}, false
	}
	literal := p.text[p.pos : p.pos+match[1]]
	p.pos += match[1]
	if match[2] < 0 && match[4] < 0 {
		if integer, err := strconv.ParseInt(literal, 10, 64); err == nil {
			return pyValue{kind: pyInt, i: integer}, true
		}
	}
	value, err := strconv.ParseFloat(literal, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return pyValue{}, false
	}
	return pyNumber(value), true
}

func (p *jsonParser) string() (string, bool) {
	p.pos++ // opening quote
	var builder strings.Builder
	for p.pos < len(p.text) {
		character := p.text[p.pos]
		switch {
		case character == '"':
			p.pos++
			return builder.String(), true
		case character < 0x20:
			return "", false // control characters are invalid with strict=True
		case character == '\\':
			if !p.escape(&builder) {
				return "", false
			}
		default:
			r, size := utf8.DecodeRuneInString(p.text[p.pos:])
			builder.WriteRune(r)
			p.pos += size
		}
	}
	return "", false
}

func (p *jsonParser) escape(builder *strings.Builder) bool {
	if p.pos+1 >= len(p.text) {
		return false
	}
	code := p.text[p.pos+1]
	p.pos += 2
	simple := map[byte]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}
	if r, ok := simple[code]; ok {
		builder.WriteRune(r)
		return true
	}
	if code != 'u' {
		return false
	}
	unit, ok := p.hex4()
	if !ok {
		return false
	}
	// A high surrogate followed by an escaped low surrogate is one character.
	// A lone surrogate becomes U+FFFD here; Python keeps it.
	if unit >= 0xd800 && unit <= 0xdbff && strings.HasPrefix(p.text[p.pos:], `\u`) {
		saved := p.pos
		p.pos += 2
		if low, ok := p.hex4(); ok && low >= 0xdc00 && low <= 0xdfff {
			builder.WriteRune(0x10000 + (unit-0xd800)<<10 + (low - 0xdc00))
			return true
		}
		p.pos = saved
	}
	builder.WriteRune(unit)
	return true
}

func (p *jsonParser) hex4() (rune, bool) {
	if p.pos+4 > len(p.text) {
		return 0, false
	}
	value, err := strconv.ParseUint(p.text[p.pos:p.pos+4], 16, 32)
	if err != nil {
		return 0, false
	}
	p.pos += 4
	return rune(value), true
}

func (p *jsonParser) array() (pyValue, bool) {
	p.pos++
	result := pyValue{kind: pyList}
	p.skipSpace()
	if p.consume("]") {
		return result, true
	}
	for {
		p.skipSpace()
		item, ok := p.value()
		if !ok {
			return pyValue{}, false
		}
		result.items = append(result.items, item)
		p.skipSpace()
		if p.consume("]") {
			return result, true
		}
		if !p.consume(",") {
			return pyValue{}, false
		}
	}
}

func (p *jsonParser) object() (pyValue, bool) {
	p.pos++
	result := pyValue{kind: pyDict, dict: map[string]pyValue{}}
	p.skipSpace()
	if p.consume("}") {
		return result, true
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.text) || p.text[p.pos] != '"' {
			return pyValue{}, false
		}
		key, ok := p.string()
		if !ok {
			return pyValue{}, false
		}
		p.skipSpace()
		if !p.consume(":") {
			return pyValue{}, false
		}
		p.skipSpace()
		item, ok := p.value()
		if !ok {
			return pyValue{}, false
		}
		if _, seen := result.dict[key]; !seen {
			result.keys = append(result.keys, key)
		}
		result.dict[key] = item
		p.skipSpace()
		if p.consume("}") {
			return result, true
		}
		if !p.consume(",") {
			return pyValue{}, false
		}
	}
}
