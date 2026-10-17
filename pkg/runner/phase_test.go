package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestLifecyclePhasesObserveExistingSuccessFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		configure   func(*rig)
		failedPhase string
		wantPhases  int
	}{
		{"success", func(*rig) {}, "", 8},
		{"prepare failure", func(r *rig) { r.benchmark.prepareErr = errors.New("PRIVATE_SENTINEL") }, "prepare", 3},
		{"execute cancellation", func(r *rig) { r.harness.runErrors = []error{context.Canceled} }, "execute", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, 1)
			tc.configure(rig)
			var logs bytes.Buffer
			rig.runner.logger.SetOutput(&logs)
			rig.runner.logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
			_, _ = rig.runner.Run(context.Background())
			pending := map[string]string{}
			starts := map[string]time.Time{}
			counts := map[string]int{}
			failed := false
			previous := ""
			lastPhase := ""
			scanner := bufio.NewScanner(&logs)
			for scanner.Scan() {
				var v map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
					t.Fatal(err)
				}
				if v["msg"] != "task phase" {
					continue
				}
				b, _ := json.Marshal(v)
				if strings.Contains(string(b), "PRIVATE_SENTINEL") {
					t.Fatal("phase event leaked raw error")
				}
				id, _ := v["phase_id"].(string)
				phase, _ := v["phase"].(string)
				if id == "" || v["task_id"] != "a" {
					t.Fatalf("missing phase identity %+v", v)
				}
				if v["depends_on"] != previous {
					t.Fatalf("phase predecessor = %v want %q", v["depends_on"], previous)
				}
				if v["event"] == "start" {
					if pending[id] != "" {
						t.Fatal("duplicate start")
					}
					pending[id] = phase
					at, err := time.Parse(time.RFC3339Nano, v["time"].(string))
					if err != nil {
						t.Fatal(err)
					}
					starts[id] = at
				} else {
					if pending[id] != phase {
						t.Fatal("unmatched end")
					}
					delete(pending, id)
					previous = id
					lastPhase = phase
					counts[phase]++
					if duration, ok := v["duration_ms"].(float64); !ok || duration < 0 {
						t.Fatal("missing measured duration")
					}
					ended, err := time.Parse(time.RFC3339Nano, v["time"].(string))
					if err != nil {
						t.Fatal(err)
					}
					measured := v["duration_ms"].(float64)
					if math.Abs(float64(ended.Sub(starts[id]))/float64(time.Millisecond)-measured) > 0.001 {
						t.Fatal("phase timestamp and duration origins differ")
					}
					if phase == tc.failedPhase && v["status"] != "succeeded" {
						failed = true
					}
				}
			}
			if lastPhase != "cleanup" {
				t.Fatal("last phase must be cleanup")
			}
			if len(pending) != 0 || len(counts) != tc.wantPhases {
				t.Fatalf("pending=%v counts=%v", pending, counts)
			}
			if tc.failedPhase != "" && !failed {
				t.Fatal("failure status missing")
			}
		})
	}
}
