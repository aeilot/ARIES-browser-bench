package browserbench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// The sandbox's browser writes one JSON line per finished or failed request
// and per page load (network_timing.js). This file turns those lines into a
// summary that separates time spent waiting on remote sites from time the
// browser spent working locally. Times are epoch milliseconds; all runtimes
// of a run share one host clock, so they compare with harness span times.

// requestTiming holds a request's phase offsets in milliseconds after its
// start, -1 for a phase that did not happen.
type requestTiming struct {
	DNSStart      float64 `json:"dns_start"`
	DNSEnd        float64 `json:"dns_end"`
	ConnectStart  float64 `json:"connect_start"`
	TLSStart      float64 `json:"tls_start"`
	ConnectEnd    float64 `json:"connect_end"`
	RequestStart  float64 `json:"request_start"`
	ResponseStart float64 `json:"response_start"`
	ResponseEnd   float64 `json:"response_end"`
}

type requestSizes struct {
	RequestHeaders  float64 `json:"request_headers"`
	RequestBody     float64 `json:"request_body"`
	ResponseHeaders float64 `json:"response_headers"`
	ResponseBody    float64 `json:"response_body"`
}

type navigationTiming struct {
	TimeOrigin          float64 `json:"time_origin"`
	RequestStart        float64 `json:"request_start"`
	ResponseStart       float64 `json:"response_start"`
	ResponseEnd         float64 `json:"response_end"`
	DOMContentLoadedEnd float64 `json:"dom_content_loaded_end"`
	LoadEnd             float64 `json:"load_end"`
	TransferSize        float64 `json:"transfer_size"`
}

type timingRecord struct {
	Type         string            `json:"type"`
	Page         int               `json:"page"`
	URL          string            `json:"url"`
	ResourceType string            `json:"resource_type"`
	Navigation   bool              `json:"navigation"`
	MainFrame    bool              `json:"main_frame"`
	StartMs      float64           `json:"start_ms"`
	EndMs        float64           `json:"end_ms"`
	EventMs      float64           `json:"event_ms"`
	Timing       requestTiming     `json:"timing"`
	Status       int               `json:"status"`
	Failure      string            `json:"failure"`
	Sizes        *requestSizes     `json:"sizes"`
	NavTiming    *navigationTiming `json:"navigation_timing"`
}

// Interval is a half-open span [StartMs, EndMs) in epoch milliseconds.
type Interval struct {
	StartMs float64 `json:"start_ms"`
	EndMs   float64 `json:"end_ms"`
}

// NavigationSummary splits one page load at the arrival of its document:
// NetworkMs is the document's request to its last byte, and AfterResponseMs
// is the rest of the load, in which the browser parses, runs scripts, lays
// out and fetches subresources.
type NavigationSummary struct {
	Page               int     `json:"page"`
	URL                string  `json:"url"`
	NetworkMs          float64 `json:"document_network_ms"`
	DOMContentLoadedMs float64 `json:"dom_content_loaded_ms"`
	LoadMs             float64 `json:"load_ms"`
	AfterResponseMs    float64 `json:"after_response_ms"`
}

// NetworkSummary is evaluation/network-summary.json.
type NetworkSummary struct {
	Requests int `json:"requests"`
	Failed   int `json:"failed"`
	// Unsent counts failed requests that never reached the network.
	Unsent         int            `json:"unsent"`
	ByResourceType map[string]int `json:"by_resource_type"`
	// ResponseBytes is headers plus bodies as Chromium reports them; requests
	// without sizes (failed ones) add nothing.
	ResponseBytes uint64 `json:"response_bytes"`
	// NetworkActiveMs is the union of all requests' lifetimes: time in which
	// at least one request was waiting on the network.
	NetworkActiveMs float64             `json:"network_active_ms"`
	FirstStartMs    float64             `json:"first_start_ms,omitempty"`
	LastEndMs       float64             `json:"last_end_ms,omitempty"`
	Navigations     []NavigationSummary `json:"navigations"`
	// MalformedLines counts lines that were not timing records.
	MalformedLines int `json:"malformed_lines"`
}

// ParseNetworkTiming reads the browser's timing lines, returning the summary
// and the request intervals for joining with harness spans.
func ParseNetworkTiming(reader io.Reader) (NetworkSummary, []Interval, error) {
	summary := NetworkSummary{ByResourceType: map[string]int{}, Navigations: []NavigationSummary{}}
	var intervals []Interval
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record timingRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			summary.MalformedLines++
			continue
		}
		switch record.Type {
		case "request":
			interval, ok := requestInterval(record)
			// A request that failed before it was sent (blocked by CSP or
			// a client, or an unresolved name) has no start time; it counts,
			// but spent no time on the network.
			unsent := !ok && record.Failure != "" && record.StartMs == 0
			if !ok && !unsent {
				summary.MalformedLines++
				continue
			}
			summary.Requests++
			summary.ByResourceType[record.ResourceType]++
			if record.Failure != "" {
				summary.Failed++
			}
			if unsent {
				summary.Unsent++
				continue
			}
			if sizes := record.Sizes; sizes != nil {
				summary.ResponseBytes += nonNegative(sizes.ResponseHeaders) + nonNegative(sizes.ResponseBody)
			}
			intervals = append(intervals, interval)
		case "load":
			if navigation, ok := navigationSummary(record); ok {
				summary.Navigations = append(summary.Navigations, navigation)
			}
		case "page":
		default:
			summary.MalformedLines++
		}
	}
	if err := scanner.Err(); err != nil {
		return NetworkSummary{}, nil, fmt.Errorf("read network timing: %w", err)
	}
	merged := MergeIntervals(intervals)
	summary.NetworkActiveMs = TotalMs(merged)
	if len(merged) > 0 {
		summary.FirstStartMs, summary.LastEndMs = merged[0].StartMs, merged[len(merged)-1].EndMs
	}
	return summary, intervals, nil
}

// requestInterval is a request's lifetime: from its start to its last
// response byte, or to the failure event for a request that never finished.
func requestInterval(record timingRecord) (Interval, bool) {
	if !finite(record.StartMs) || record.StartMs <= 0 {
		return Interval{}, false
	}
	end := record.EndMs
	if record.Timing.ResponseEnd >= 0 && finite(record.Timing.ResponseEnd) {
		end = record.StartMs + record.Timing.ResponseEnd
	}
	if !finite(end) || end < record.StartMs {
		return Interval{}, false
	}
	return Interval{StartMs: record.StartMs, EndMs: end}, true
}

func navigationSummary(record timingRecord) (NavigationSummary, bool) {
	navigation := record.NavTiming
	if navigation == nil || navigation.ResponseEnd <= 0 || navigation.LoadEnd < navigation.ResponseEnd {
		return NavigationSummary{}, false
	}
	return NavigationSummary{
		Page: record.Page, URL: record.URL,
		NetworkMs:          navigation.ResponseEnd - math.Max(navigation.RequestStart, 0),
		DOMContentLoadedMs: navigation.DOMContentLoadedEnd,
		LoadMs:             navigation.LoadEnd,
		AfterResponseMs:    navigation.LoadEnd - navigation.ResponseEnd,
	}, true
}

// MergeIntervals returns the union of intervals as sorted, disjoint spans.
func MergeIntervals(intervals []Interval) []Interval {
	sorted := make([]Interval, 0, len(intervals))
	for _, interval := range intervals {
		if interval.EndMs > interval.StartMs {
			sorted = append(sorted, interval)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartMs < sorted[j].StartMs })
	var merged []Interval
	for _, interval := range sorted {
		if last := len(merged) - 1; last >= 0 && interval.StartMs <= merged[last].EndMs {
			merged[last].EndMs = math.Max(merged[last].EndMs, interval.EndMs)
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

// TotalMs is the summed length of disjoint intervals.
func TotalMs(intervals []Interval) float64 {
	total := 0.0
	for _, interval := range intervals {
		total += interval.EndMs - interval.StartMs
	}
	return total
}

// OverlapMs is the length of the intersection of two interval sets.
func OverlapMs(a, b []Interval) float64 {
	a, b = MergeIntervals(a), MergeIntervals(b)
	total := 0.0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		start, end := math.Max(a[i].StartMs, b[j].StartMs), math.Min(a[i].EndMs, b[j].EndMs)
		if end > start {
			total += end - start
		}
		if a[i].EndMs < b[j].EndMs {
			i++
		} else {
			j++
		}
	}
	return total
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func nonNegative(value float64) uint64 {
	if !finite(value) || value <= 0 {
		return 0
	}
	return uint64(value)
}

// Span is one named harness span, such as a tool or model call.
type Span struct {
	Name string
	Interval
}

// TimeSplit divides a task's time between waiting on remote sites, local
// browser work, and the model, from harness spans and the browser's request
// intervals.
type TimeSplit struct {
	// BrowserCalls counts browser tool calls; BrowserCallMs is the union of
	// their spans.
	BrowserCalls  int     `json:"browser_calls"`
	BrowserCallMs float64 `json:"browser_call_ms"`
	// RemoteWaitMs is browser call time in which at least one request was on
	// the network; LocalMs is the rest of it: rendering, script execution,
	// snapshotting, and the tool protocol's own overhead.
	RemoteWaitMs float64 `json:"remote_wait_ms"`
	LocalMs      float64 `json:"local_ms"`
	// NetworkOutsideCallsMs is network activity between browser calls, such
	// as a page that keeps loading after a call returned.
	NetworkOutsideCallsMs float64 `json:"network_outside_calls_ms"`
	ModelCalls            int     `json:"model_calls"`
	ModelMs               float64 `json:"model_ms"`
	// SpanMs runs from the first span's start to the last span's end.
	SpanMs float64 `json:"span_ms"`
}

// SplitTime computes a TimeSplit. isBrowserCall and isModelCall select the
// spans of each kind by name.
func SplitTime(spans []Span, network []Interval, isBrowserCall, isModelCall func(string) bool) TimeSplit {
	var split TimeSplit
	var browser, model []Interval
	first, last := math.Inf(1), math.Inf(-1)
	for _, span := range spans {
		if span.EndMs < span.StartMs {
			continue
		}
		first, last = math.Min(first, span.StartMs), math.Max(last, span.EndMs)
		switch {
		case isBrowserCall(span.Name):
			split.BrowserCalls++
			browser = append(browser, span.Interval)
		case isModelCall(span.Name):
			split.ModelCalls++
			model = append(model, span.Interval)
		}
	}
	if len(spans) > 0 && last >= first {
		split.SpanMs = last - first
	}
	browser = MergeIntervals(browser)
	split.BrowserCallMs = TotalMs(browser)
	split.RemoteWaitMs = OverlapMs(browser, network)
	split.LocalMs = split.BrowserCallMs - split.RemoteWaitMs
	split.NetworkOutsideCallsMs = TotalMs(MergeIntervals(network)) - split.RemoteWaitMs
	split.ModelMs = TotalMs(MergeIntervals(model))
	return split
}
