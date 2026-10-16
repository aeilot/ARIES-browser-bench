package browserbench

import (
	"math"
	"regexp"
	"strings"
)

// This file ports AssistantBench's question_scorer as vendored by BrowserGym
// at ServiceNow/BrowserGym@f0e4275d706f31aa341e58e49a5effbb52a392a8
// (browsergym/assistantbench/src/browsergym/assistantbench/evaluation), the
// scorer BrowserGym's AssistantBench task calls on the agent's final answer.
// Function names follow the Python ones. Where the Python raises, the port
// returns an error wrapping errPyRaised; scoreAnswer reports that as a zero
// score with the reason, since an upstream run would fail the task there.

// scoreAnswer is question_scorer(prediction, gold_answer)[0], the accuracy
// in [0, 1].
func scoreAnswer(prediction, gold string) (float64, error) {
	predicted, ok := pyJSONLoads(prediction)
	if !ok {
		predicted = pyString(prediction)
	}
	var answerList []string
	for _, line := range strings.Split(gold, "\n") {
		if len(pyStrip(line)) > 0 {
			answerList = append(answerList, line)
		}
	}
	goldAnswer, evaluator := parseAnswer(answerList)
	predicted, runEval := fixPrediction(predicted, goldAnswer, evaluator)

	// has_ans is discarded, but computing it raises for some predictions.
	if predicted.kind != pyFloat && !predicted.hasLen() {
		return 0, pyRaise("len() of %s", predicted.typeName())
	}
	if predicted.kind == pyList {
		for _, item := range predicted.items {
			if item.kind == pyInt || item.kind == pyFloat {
				if !(item.kind == pyFloat && math.IsNaN(item.f)) {
					break
				}
				continue
			}
			if !item.hasLen() {
				return 0, pyRaise("len() of %s", item.typeName())
			}
			if item.length() != 0 {
				break
			}
		}
	}
	if !runEval {
		return 0, nil
	}
	switch evaluator {
	case "number":
		return evaluateNumbers(predicted, goldAnswer)
	case "json":
		return evaluateDicts(predicted, goldAnswer)
	default: // "string" and "string list"
		return evaluateStrings(predicted, goldAnswer), nil
	}
}

// fixAns is evaluator.fix_ans: single-quoted dict syntax to JSON quoting.
func fixAns(answer string) string {
	answer = strings.ReplaceAll(answer, "{'", `{"`)
	answer = strings.ReplaceAll(answer, "', '", `", "`)
	answer = strings.ReplaceAll(answer, "': '", `": "`)
	answer = strings.ReplaceAll(answer, "'}", `"}`)
	return strings.ReplaceAll(answer, "': ", `": `)
}

// parseAnswer is evaluator.parse_answer on the gold answer's lines.
func parseAnswer(answer []string) (pyValue, string) {
	if len(answer) == 1 {
		if value, isNumber := fixNumber(pyString(answer[0]), true); isNumber {
			return value, "number"
		}
		if value, ok := pyJSONLoads(fixAns(answer[0])); ok {
			return pyValue{kind: pyList, items: []pyValue{value}}, "json"
		}
		return pyString(answer[0]), "string"
	}
	parsed := pyValue{kind: pyList}
	for _, line := range answer {
		value, ok := pyJSONLoads(fixAns(line))
		if !ok {
			spans := pyValue{kind: pyList}
			for _, item := range answer {
				spans.items = append(spans.items, pyString(item))
			}
			return spans, "string list"
		}
		parsed.items = append(parsed.items, value)
	}
	return parsed, "json"
}

// fixNumber is fix_number. The evaluator module's version also removes
// " square kilometers"; evaluate_dicts' version does not. Both report
// non-string, non-int values as numbers unchanged, which callers ignore.
func fixNumber(number pyValue, squareKilometers bool) (pyValue, bool) {
	switch number.kind {
	case pyStr:
		text := strings.NewReplacer("$", " ", "%", " ", "sqft", " ").Replace(number.s)
		text = strings.ReplaceAll(pyStrip(text), ",", ".")
		if squareKilometers {
			text = strings.ReplaceAll(text, " square kilometers", "")
		}
		if value, ok := pyParseFloat(text); ok {
			return pyNumber(value), true
		}
		return number, false
	case pyInt:
		return pyNumber(float64(number.i)), true
	default:
		return number, true
	}
}

// fixPrediction is evaluator.fix_prediction.
func fixPrediction(prediction, goldAnswer pyValue, evaluator string) (pyValue, bool) {
	if prediction.kind == pyList && len(prediction.items) == 1 {
		first := prediction.items[0]
		if first.kind == pyInt || first.kind == pyStr && pyIsNumeric(first.s) {
			// fix_number returns a (value, is_number) tuple, which the
			// Python assigns to prediction as it is.
			value, isNumber := fixNumber(first, true)
			prediction = pyValue{kind: pyTuple, items: []pyValue{value, {kind: pyBool, b: isNumber}}}
		}
	}
	if prediction.kind != pyList {
		prediction, _ = fixNumber(prediction, true)
		if evaluator == "json" {
			prediction = splitJSONLines(prediction)
		}
	}
	if prediction.hasLen() && prediction.length() == 0 {
		return prediction, false
	}
	if prediction.kind == pyList && len(prediction.items) > 1 && goldAnswer.kind == pyFloat {
		return prediction, false
	}
	return prediction, true
}

// splitJSONLines is [json.loads(pred) for pred in prediction.split("\n")],
// falling back to [prediction] when prediction is not a string or any line
// is not JSON.
func splitJSONLines(prediction pyValue) pyValue {
	fallback := pyValue{kind: pyList, items: []pyValue{prediction}}
	if prediction.kind != pyStr {
		return fallback
	}
	result := pyValue{kind: pyList}
	for _, line := range strings.Split(prediction.s, "\n") {
		value, ok := pyJSONLoads(line)
		if !ok {
			return fallback
		}
		result.items = append(result.items, value)
	}
	return result
}

// evaluateNumbers is evaluate_numbers.evaluate_numbers.
func evaluateNumbers(prediction, gold pyValue) (float64, error) {
	predicted, ok, err := pyFloatOf(prediction)
	if err != nil {
		return 0, err
	}
	expected, goldOK, err := pyFloatOf(gold)
	if err != nil {
		return 0, err
	}
	if !ok || !goldOK {
		return 0, nil
	}
	return distanceFunctionLog(predicted, expected), nil
}

// pyFloatOf is float(value) for a value that is not already a float or int:
// a ValueError is reported as !ok, a TypeError as an error.
func pyFloatOf(value pyValue) (float64, bool, error) {
	switch value.kind {
	case pyFloat:
		return value.f, true, nil
	case pyInt:
		return float64(value.i), true, nil
	case pyBool:
		if value.b {
			return 1, true, nil
		}
		return 0, true, nil
	case pyStr:
		parsed, ok := pyParseFloat(value.s)
		return parsed, ok, nil
	default:
		return 0, false, pyRaise("float() of %s", value.typeName())
	}
}

// distanceFunctionLog is distance_function_log. max(0, nan) is 0 in Python,
// as it is here.
func distanceFunctionLog(prediction, gold float64) float64 {
	if prediction == gold && gold == 0 {
		return 1
	}
	if prediction == 0 {
		prediction = 1e-4
	}
	if gold == 0 {
		gold = 1e-4
	}
	var score float64
	if prediction > gold {
		score = 1 - math.Log(prediction/gold)
	} else {
		score = 1 - math.Log(gold/prediction)
	}
	if score > 0 {
		return score
	}
	return 0
}

// evaluateStrings is evaluate_strings.evaluate_strings, the DROP F1 over
// optimally aligned answer spans. Any exception inside it scores 0.
func evaluateStrings(prediction, gold pyValue) float64 {
	predictedSpans, ok := stringSpans(prediction)
	if !ok {
		return 0
	}
	goldSpans, ok := stringSpans(gold)
	if !ok {
		return 0
	}
	predictedBags := answerToBags(predictedSpans)
	goldBags := answerToBags(goldSpans)
	scores := make([][]float64, len(goldBags))
	for goldIndex, goldBag := range goldBags {
		scores[goldIndex] = make([]float64, len(predictedBags))
		for predictedIndex, predictedBag := range predictedBags {
			if matchNumbersIfPresent(goldBag, predictedBag) {
				scores[goldIndex][predictedIndex] = computeF1(predictedBag, goldBag)
			}
		}
	}
	return alignedMean(scores, len(goldBags), len(predictedBags))
}

// stringSpans is str() of a non-list value, or a list's elements, which must
// all be strings for _normalize_answer not to raise.
func stringSpans(value pyValue) ([]string, bool) {
	if value.kind != pyList {
		return []string{pyToStr(value)}, true
	}
	spans := make([]string, len(value.items))
	for index, item := range value.items {
		if item.kind != pyStr {
			return nil, false
		}
		spans[index] = item.s
	}
	return spans, true
}

func answerToBags(spans []string) []map[string]struct{} {
	bags := make([]map[string]struct{}, len(spans))
	for index, span := range spans {
		bag := map[string]struct{}{}
		for _, token := range pySplitWhitespace(normalizeAnswer(span)) {
			bag[token] = struct{}{}
		}
		bags[index] = bag
	}
	return bags
}

// tokenSeparator is _tokenize's re.split(" |-", text).
var tokenSeparator = regexp.MustCompile(" |-")

// pythonPunctuation is string.punctuation.
const pythonPunctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

func normalizeAnswer(text string) string {
	var parts []string
	for _, token := range tokenSeparator.Split(text, -1) {
		token = strings.ToLower(token)
		if _, isNumber := pyParseFloat(token); !isNumber {
			token = strings.Map(func(r rune) rune {
				if r < 0x80 && strings.ContainsRune(pythonPunctuation, r) {
					return -1
				}
				return r
			}, token)
		}
		if value, isNumber := pyParseFloat(token); isNumber {
			token = pyFloatRepr(value)
		}
		token = strings.Join(pySplitWhitespace(removeArticles(token)), " ")
		if pyStrip(token) != "" {
			parts = append(parts, token)
		}
	}
	return pyStrip(strings.Join(parts, " "))
}

// removeArticles is re.sub(r"\b(a|an|the)\b", " ", text) with Unicode word
// boundaries, which Go's regexp does not provide.
func removeArticles(text string) string {
	runes := []rune(text)
	var builder strings.Builder
	for index := 0; index < len(runes); {
		if index == 0 || !pyIsWordChar(runes[index-1]) {
			matched := 0
			for _, article := range []string{"a", "an", "the"} {
				end := index + len(article)
				if end <= len(runes) && string(runes[index:end]) == article && (end == len(runes) || !pyIsWordChar(runes[end])) {
					matched = len(article)
					break
				}
			}
			if matched > 0 {
				builder.WriteByte(' ')
				index += matched
				continue
			}
		}
		builder.WriteRune(runes[index])
		index++
	}
	return builder.String()
}

func computeF1(predictedBag, goldBag map[string]struct{}) float64 {
	intersection := 0
	for token := range goldBag {
		if _, ok := predictedBag[token]; ok {
			intersection++
		}
	}
	precision, recall := 1.0, 1.0
	if len(predictedBag) > 0 {
		precision = float64(intersection) / float64(len(predictedBag))
	}
	if len(goldBag) > 0 {
		recall = float64(intersection) / float64(len(goldBag))
	}
	if precision == 0 && recall == 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}

func matchNumbersIfPresent(goldBag, predictedBag map[string]struct{}) bool {
	goldNumbers := false
	for word := range goldBag {
		if _, isNumber := pyParseFloat(word); isNumber {
			goldNumbers = true
			if _, ok := predictedBag[word]; ok {
				return true
			}
		}
	}
	return !goldNumbers
}

// evaluateDicts is evaluate_dicts.evaluate_dicts.
func evaluateDicts(prediction, gold pyValue) (float64, error) {
	// fixPrediction leaves a list here and parseAnswer a list of gold values;
	// the other shapes are handled only as far as the Python would get.
	switch {
	case prediction.kind == pyDict:
		// Iterating a dict yields its keys, on which calc_recall raises.
		if len(prediction.keys) > 0 && len(gold.items) > 0 {
			return 0, pyRaise("calc_recall on a dict key")
		}
		return alignedMean(nil, len(gold.items), len(prediction.keys)), nil
	case !prediction.hasLen():
		return 0, pyRaise("len() of %s", prediction.typeName())
	case prediction.length() == 0:
		return alignedMean(nil, len(gold.items), 0), nil
	case prediction.kind != pyList || prediction.items[0].kind != pyDict:
		return 0, nil
	}
	scores := make([][]float64, len(gold.items))
	for goldIndex, goldItem := range gold.items {
		scores[goldIndex] = make([]float64, len(prediction.items))
		for predictedIndex, predictedItem := range prediction.items {
			score, err := evaluatePairOfDicts(predictedItem, goldItem)
			if err != nil {
				return 0, err
			}
			if math.IsNaN(score) {
				return 0, pyRaise("linear_sum_assignment on a NaN score")
			}
			scores[goldIndex][predictedIndex] = score
		}
	}
	return alignedMean(scores, len(gold.items), len(prediction.items)), nil
}

func evaluatePairOfDicts(prediction, gold pyValue) (float64, error) {
	recall, err := calcRecall(prediction, gold, true)
	if err != nil {
		return 0, err
	}
	precision, err := calcRecall(gold, prediction, false)
	if err != nil {
		return 0, err
	}
	if precision+recall == 0 {
		return 0, nil
	}
	return 2 * precision * recall / (precision + recall), nil
}

// calcRecall is evaluate_dicts.calc_recall; np.average of no values is NaN.
func calcRecall(prediction, gold pyValue, useGoldForEval bool) (float64, error) {
	if gold.kind != pyDict {
		return 0, pyRaise("%s has no items()", gold.typeName())
	}
	if prediction.kind != pyDict && len(gold.keys) > 0 {
		return 0, pyRaise("%s has no get()", prediction.typeName())
	}
	var recall []float64
	for _, key := range gold.keys {
		goldValue, _ := fixNumber(gold.dict[key], false)
		predictedRaw, present := prediction.dict[key]
		predictedValue, _ := fixNumber(predictedRaw, false) // pred.get() is None when absent
		if !present {
			recall = append(recall, 0)
			continue
		}
		selector := predictedValue
		if useGoldForEval {
			selector = goldValue
		}
		var evaluate func(pyValue, pyValue) (float64, error)
		switch selector.kind {
		case pyStr, pyBool, pyList:
			evaluate = func(p, g pyValue) (float64, error) { return evaluateStrings(p, g), nil }
		case pyInt, pyFloat:
			evaluate = evaluateNumbers
		default:
			return 0, pyRaise("no evaluator for %s", selector.typeName())
		}
		if predictedValue.kind != goldValue.kind {
			recall = append(recall, 0)
			continue
		}
		score, err := evaluate(predictedValue, goldValue)
		if err != nil {
			return 0, err
		}
		recall = append(recall, score)
	}
	if len(recall) == 0 {
		return math.NaN(), nil
	}
	sum := 0.0
	for _, value := range recall {
		sum += value
	}
	return sum / float64(len(recall)), nil
}

// alignedMean is the mean of _align_bags: the best one-to-one assignment
// of predictions to gold rows, averaged over the larger of the two counts.
// Only the assignment's total enters the mean, so ties between optimal
// assignments do not change it.
func alignedMean(scores [][]float64, rows, columns int) float64 {
	size := max(rows, columns)
	if size == 0 {
		return math.NaN()
	}
	return maxAssignment(scores, rows, columns) / float64(size)
}

// maxAssignment returns the largest total of a one-to-one assignment
// between rows and columns, padding the matrix square with zeros. It is the
// Hungarian algorithm on negated scores.
func maxAssignment(scores [][]float64, rows, columns int) float64 {
	size := max(rows, columns)
	cost := func(row, column int) float64 {
		if row < rows && column < columns {
			return -scores[row][column]
		}
		return 0
	}
	u := make([]float64, size+1)
	v := make([]float64, size+1)
	match := make([]int, size+1) // match[column] = row, 1-based
	way := make([]int, size+1)
	for row := 1; row <= size; row++ {
		match[0] = row
		column0 := 0
		minimum := make([]float64, size+1)
		used := make([]bool, size+1)
		for index := range minimum {
			minimum[index] = math.Inf(1)
		}
		for {
			used[column0] = true
			row0, delta, column1 := match[column0], math.Inf(1), 0
			for column := 1; column <= size; column++ {
				if used[column] {
					continue
				}
				current := cost(row0-1, column-1) - u[row0] - v[column]
				if current < minimum[column] {
					minimum[column], way[column] = current, column0
				}
				if minimum[column] < delta {
					delta, column1 = minimum[column], column
				}
			}
			for column := 0; column <= size; column++ {
				if used[column] {
					u[match[column]] += delta
					v[column] -= delta
				} else {
					minimum[column] -= delta
				}
			}
			column0 = column1
			if match[column0] == 0 {
				break
			}
		}
		for column0 != 0 {
			column1 := way[column0]
			match[column0] = match[column1]
			column0 = column1
		}
	}
	total := 0.0
	for column := 1; column <= size; column++ {
		if row := match[column]; row > 0 && row <= rows && column <= columns {
			total += scores[row-1][column-1]
		}
	}
	return total
}
