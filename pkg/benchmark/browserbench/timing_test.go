package browserbench

import (
	"math"
	"strings"
	"testing"
)

func TestParseNetworkTimingSummarizesRequestsAndLoads(t *testing.T) {
	records := strings.Join([]string{
		`{"type":"page","page":1,"event_ms":900}`,
		// Document: 0..250 ms after start at 1000.
		`{"type":"request","page":1,"resource_type":"document","navigation":true,"main_frame":true,"start_ms":1000,"end_ms":1260,"timing":{"dns_start":0,"dns_end":10,"connect_start":10,"tls_start":20,"connect_end":40,"request_start":41,"response_start":200,"response_end":250},"status":200,"sizes":{"request_headers":300,"request_body":0,"response_headers":100,"response_body":5000}}`,
		// Overlaps the document: 1200..1400.
		`{"type":"request","page":1,"resource_type":"script","start_ms":1200,"end_ms":1410,"timing":{"response_end":200},"status":200,"sizes":{"response_headers":50,"response_body":-1}}`,
		// Failed without a response: lasts to its failure event, 2000..2100.
		`{"type":"request","page":1,"resource_type":"image","start_ms":2000,"end_ms":2100,"timing":{"response_end":-1},"failure":"net::ERR_ABORTED"}`,
		`{"type":"load","page":1,"url":"https://example.org/","event_ms":1500,"navigation_timing":{"time_origin":1000,"request_start":41,"response_start":200,"response_end":250,"dom_content_loaded_end":320,"load_end":480,"transfer_size":5100}}`,
		`{"type":"load","page":1,"url":"about:blank","event_ms":1600,"navigation_timing":null}`,
		// Blocked before it was sent: counted, with no network time.
		`{"type":"request","page":1,"resource_type":"image","start_ms":0,"end_ms":2500,"timing":{"response_end":-1},"failure":"csp"}`,
		`not json`,
		`{"type":"request","start_ms":0}`,
	}, "\n")
	summary, intervals, err := ParseNetworkTiming(strings.NewReader(records))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 4 || summary.Failed != 2 || summary.Unsent != 1 || summary.ByResourceType["image"] != 2 || len(intervals) != 3 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.ResponseBytes != 5150 {
		t.Fatalf("response bytes = %d", summary.ResponseBytes)
	}
	// Union of [1000,1250), [1200,1400) and [2000,2100) is 400 + 100 ms.
	if summary.NetworkActiveMs != 500 || summary.FirstStartMs != 1000 || summary.LastEndMs != 2100 {
		t.Fatalf("network active = %+v", summary)
	}
	if len(summary.Navigations) != 1 {
		t.Fatalf("navigations = %+v", summary.Navigations)
	}
	navigation := summary.Navigations[0]
	if navigation.NetworkMs != 209 || navigation.AfterResponseMs != 230 || navigation.LoadMs != 480 {
		t.Fatalf("navigation = %+v", navigation)
	}
	if summary.MalformedLines != 2 {
		t.Fatalf("malformed lines = %d", summary.MalformedLines)
	}
}

func TestIntervalArithmetic(t *testing.T) {
	calls := []Interval{{0, 100}, {200, 300}}
	network := []Interval{{50, 250}, {90, 120}, {400, 500}}
	if merged := MergeIntervals(network); len(merged) != 2 || merged[0] != (Interval{50, 250}) {
		t.Fatalf("merged = %v", merged)
	}
	// Calls cover 0..100 and 200..300; the network is busy 50..250.
	if overlap := OverlapMs(calls, network); math.Abs(overlap-100) > 1e-9 {
		t.Fatalf("overlap = %v", overlap)
	}
	if total := TotalMs(MergeIntervals(calls)); total != 200 {
		t.Fatalf("total = %v", total)
	}
	if overlap := OverlapMs(nil, network); overlap != 0 {
		t.Fatalf("empty overlap = %v", overlap)
	}
}

func TestSplitTimeSeparatesRemoteWaitFromLocalWork(t *testing.T) {
	spans := []Span{
		{Name: "api.model", Interval: Interval{0, 1000}},
		{Name: "MCP send tools/call browser_navigate", Interval: Interval{1000, 2000}},
		{Name: "MCP send tools/call browser_snapshot", Interval: Interval{2500, 2600}},
		{Name: "tool.terminal", Interval: Interval{2600, 2700}},
	}
	// The navigation waits on the network for 700 ms of its 1000; a request
	// keeps loading 200 ms after it returned.
	network := []Interval{{1100, 1500}, {1300, 1700}, {1900, 2200}}
	split := SplitTime(spans, network,
		func(name string) bool { return strings.HasPrefix(name, "MCP send tools/call browser_") },
		func(name string) bool { return strings.HasPrefix(name, "api.") })
	want := TimeSplit{BrowserCalls: 2, BrowserCallMs: 1100, RemoteWaitMs: 700, LocalMs: 400, NetworkOutsideCallsMs: 200, ModelCalls: 1, ModelMs: 1000, SpanMs: 2700}
	if split != want {
		t.Fatalf("split = %+v, want %+v", split, want)
	}
}
