package browserbench

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
)

// TestScoreAnswerMatchesUpstream replays question_scorer results recorded
// from the pinned Python scorer (see testdata/gen_scores.py).
func TestScoreAnswerMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/assistantbench_scores.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Prediction string   `json:"prediction"`
		Gold       string   `json:"gold"`
		Score      *float64 `json:"score"`
		NonFinite  string   `json:"nonfinite"`
		Raises     string   `json:"raises"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 2000 {
		t.Fatalf("only %d recorded cases", len(cases))
	}
	for _, c := range cases {
		got, err := scoreAnswer(c.Prediction, c.Gold)
		switch {
		case c.Raises != "":
			if !errors.Is(err, errPyRaised) {
				t.Errorf("scoreAnswer(%q, %q) = %v, %v; upstream raises %s", c.Prediction, c.Gold, got, err, c.Raises)
			}
		case err != nil:
			t.Errorf("scoreAnswer(%q, %q) error %v; upstream scores %v", c.Prediction, c.Gold, err, c.Score)
		case c.NonFinite != "":
			if pyFloatRepr(got) != c.NonFinite {
				t.Errorf("scoreAnswer(%q, %q) = %v; upstream %s", c.Prediction, c.Gold, got, c.NonFinite)
			}
		case c.Score == nil || math.Abs(got-*c.Score) > 1e-9:
			t.Errorf("scoreAnswer(%q, %q) = %v; upstream %v", c.Prediction, c.Gold, got, *c.Score)
		}
	}
}

func TestPyFloatRepr(t *testing.T) {
	for value, want := range map[float64]string{
		45: "45.0", 14.2: "14.2", 1e15: "1000000000000000.0", 1e16: "1e+16", 1.5e-5: "1.5e-05",
		0.0001: "0.0001", 1e100: "1e+100", -0.5: "-0.5", 123456789012345680: "1.2345678901234568e+17",
		math.Inf(1): "inf",
	} {
		if got := pyFloatRepr(value); got != want {
			t.Errorf("pyFloatRepr(%v) = %q, want %q", value, got, want)
		}
	}
	if got := pyFloatRepr(math.Copysign(0, -1)); got != "-0.0" {
		t.Errorf("pyFloatRepr(-0) = %q", got)
	}
}

func TestMaxAssignmentFindsOptimum(t *testing.T) {
	scores := [][]float64{{0.9, 0.8, 0}, {0.85, 0, 0}}
	// Greedy row-wise picks 0.9 then nothing better than 0; the optimum
	// pairs row 0 with column 1 and row 1 with column 0.
	if got := maxAssignment(scores, 2, 3); math.Abs(got-1.65) > 1e-12 {
		t.Fatalf("maxAssignment = %v, want 1.65", got)
	}
}
