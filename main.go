package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	lua "github.com/yuin/gopher-lua"
)

// headerFlags implements flag.Value so -H can be specified multiple times.
type headerFlags []string

func (h *headerFlags) String() string { return strings.Join(*h, ", ") }
func (h *headerFlags) Set(val string) error {
	*h = append(*h, val)
	return nil
}

var (
	targetURL    string
	requests     int
	concurrency  int
	method       string
	timeout      time.Duration
	retries      int
	headers      headerFlags
	bodyStr      string
	bodyFile     string
	duration     time.Duration
	outputFormat string
	// Phase 3
	rateLimit    int
	scriptFile   string
	thresholdP99 time.Duration
	thresholdP95 time.Duration
	thresholdErr float64
)

func init() {
	flag.StringVar(&targetURL, "url", "", "Target URL (required)")
	flag.IntVar(&requests, "n", -1, "Number of requests (default 100; mutually exclusive with -d)")
	flag.IntVar(&concurrency, "c", 10, "Number of concurrent workers")
	flag.StringVar(&method, "method", "GET", "HTTP method (GET, POST, PUT, DELETE, PATCH, HEAD)")
	flag.DurationVar(&timeout, "timeout", 10*time.Second, "Request timeout")
	flag.IntVar(&retries, "retries", 3, "Number of retries per request")
	flag.Var(&headers, "H", `Request header, e.g. -H "Authorization: Bearer token" (repeatable)`)
	flag.StringVar(&bodyStr, "body", "", "Request body string (e.g. for POST/PUT)")
	flag.StringVar(&bodyFile, "body-file", "", "Path to file to use as request body")
	flag.DurationVar(&duration, "d", 0, "Run for a fixed duration, e.g. -d 30s (mutually exclusive with -n)")
	flag.StringVar(&outputFormat, "output", "text", "Output format: text, json, or csv")
	flag.IntVar(&rateLimit, "rps", 0, "Max requests per second (0 = unlimited)")
	flag.StringVar(&scriptFile, "script", "", "Path to Lua script with pre_request/post_response hooks")
	flag.DurationVar(&thresholdP99, "threshold-p99", 0, "Fail if P99 latency exceeds this (e.g. 200ms)")
	flag.DurationVar(&thresholdP95, "threshold-p95", 0, "Fail if P95 latency exceeds this (e.g. 500ms)")
	flag.Float64Var(&thresholdErr, "threshold-err", -1, "Fail if error rate % exceeds this (e.g. 5)")
}

// --- Result & stats types ---

type Result struct {
	Latency time.Duration
	Status  int
	Error   error
	Body    []byte // populated only when Lua post_response hook exists
}

// recentEntry tracks a single recent request for the activity log.
type recentEntry struct {
	status    int
	latencyMS float64
	isError   bool
}

const maxRecentEntries = 12

// RunningStats holds live counters updated by the result-collection goroutine.
type RunningStats struct {
	mu           sync.Mutex
	completed    int
	errCount     int
	lastSnapshot int
	latencies    []float64
	statusCodes  map[int]int
	startTime    time.Time
	recent       []recentEntry // ring buffer, capped at maxRecentEntries
}

func newRunningStats(start time.Time) *RunningStats {
	return &RunningStats{
		statusCodes: make(map[int]int),
		startTime:   start,
		recent:      make([]recentEntry, 0, maxRecentEntries),
	}
}

func (rs *RunningStats) record(r Result) {
	ms := r.Latency.Seconds() * 1000
	rs.mu.Lock()
	rs.completed++
	isErr := r.Error != nil
	if isErr {
		rs.errCount++
	} else {
		rs.statusCodes[r.Status]++
	}
	rs.latencies = append(rs.latencies, ms)
	// Ring buffer for recent entries
	entry := recentEntry{status: r.Status, latencyMS: ms, isError: isErr}
	if len(rs.recent) < maxRecentEntries {
		rs.recent = append(rs.recent, entry)
	} else {
		copy(rs.recent, rs.recent[1:])
		rs.recent[maxRecentEntries-1] = entry
	}
	rs.mu.Unlock()
}

// statsSnapshot holds a point-in-time copy of RunningStats with pre-computed percentiles.
type statsSnapshot struct {
	completed   int
	errCount    int
	latencies   []float64
	statusCodes map[int]int
	elapsed     time.Duration
	instantRPS  int
	totalRPS    float64
	avg         float64
	p50         float64
	p95         float64
	p99         float64
	recent      []recentEntry
}

func (rs *RunningStats) snapshotFull() statsSnapshot {
	rs.mu.Lock()
	lats := make([]float64, len(rs.latencies))
	copy(lats, rs.latencies)
	codes := make(map[int]int, len(rs.statusCodes))
	for k, v := range rs.statusCodes {
		codes[k] = v
	}
	completed := rs.completed
	errCount := rs.errCount
	rps := completed - rs.lastSnapshot
	rs.lastSnapshot = completed
	elapsed := time.Since(rs.startTime)
	rec := make([]recentEntry, len(rs.recent))
	copy(rec, rs.recent)
	rs.mu.Unlock()

	var avg, p50v, p95v, p99v float64
	if len(lats) > 0 {
		sort.Float64s(lats)
		avg = avgFloat(lats)
		p50v = pctSorted(lats, 50)
		p95v = pctSorted(lats, 95)
		p99v = pctSorted(lats, 99)
	}

	totalRPS := 0.0
	if elapsed.Seconds() > 0 {
		totalRPS = float64(completed) / elapsed.Seconds()
	}

	return statsSnapshot{
		completed:   completed,
		errCount:    errCount,
		latencies:   lats,
		statusCodes: codes,
		elapsed:     elapsed,
		instantRPS:  rps,
		totalRPS:    totalRPS,
		avg:         avg,
		p50:         p50v,
		p95:         p95v,
		p99:         p99v,
		recent:      rec,
	}
}

// snapshot returns a simple view for the plain stats display.
func (rs *RunningStats) snapshot() (completed, lastSnap, errCount int, lats []float64) {
	rs.mu.Lock()
	completed = rs.completed
	lastSnap = rs.lastSnapshot
	errCount = rs.errCount
	lats = make([]float64, len(rs.latencies))
	copy(lats, rs.latencies)
	rs.mu.Unlock()
	return
}

func (rs *RunningStats) advanceSnapshot() {
	rs.mu.Lock()
	rs.lastSnapshot = rs.completed
	rs.mu.Unlock()
}

type JSONOutput struct {
	Config      JSONConfig     `json:"config"`
	Summary     JSONSummary    `json:"summary"`
	StatusCodes map[string]int `json:"status_codes"`
	Latency     JSONLatency    `json:"latency_ms"`
}

type JSONConfig struct {
	URL         string  `json:"url"`
	Method      string  `json:"method"`
	Concurrency int     `json:"concurrency"`
	Requests    int     `json:"requests,omitempty"`
	Duration    string  `json:"duration,omitempty"`
	Retries     int     `json:"retries"`
	TimeoutSec  float64 `json:"timeout_sec"`
}

type JSONSummary struct {
	TotalRequests  int     `json:"total_requests"`
	Succeeded      int     `json:"succeeded"`
	Failed         int     `json:"failed"`
	TotalTimeSec   float64 `json:"total_time_sec"`
	RequestsPerSec float64 `json:"requests_per_sec"`
}

type JSONLatency struct {
	AvgMs float64 `json:"avg"`
	MinMs float64 `json:"min"`
	MaxMs float64 `json:"max"`
	P50Ms float64 `json:"p50"`
	P95Ms float64 `json:"p95"`
	P99Ms float64 `json:"p99"`
}

// --- Lua helpers ---

func buildRequestTable(L *lua.LState, hdrs []string, body []byte) *lua.LTable {
	rr := L.NewTable()
	hdrTable := L.NewTable()
	for _, h := range hdrs {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			hdrTable.RawSetString(strings.TrimSpace(parts[0]), lua.LString(strings.TrimSpace(parts[1])))
		}
	}
	rr.RawSetString("headers", hdrTable)
	rr.RawSetString("body", lua.LString(body))
	return rr
}

func buildResponseTable(L *lua.LState, status int, body []byte) *lua.LTable {
	rr := L.NewTable()
	rr.RawSetString("status", lua.LNumber(status))
	rr.RawSetString("body", lua.LString(body))
	return rr
}

func extractHeaders(rr *lua.LTable) []string {
	hdrVal := rr.RawGetString("headers")
	hdrTable, ok := hdrVal.(*lua.LTable)
	if !ok {
		return nil
	}
	var result []string
	hdrTable.ForEach(func(k, v lua.LValue) {
		result = append(result, fmt.Sprintf("%s: %s", lua.LVAsString(k), lua.LVAsString(v)))
	})
	return result
}

func extractBody(rr *lua.LTable) []byte {
	return []byte(lua.LVAsString(rr.RawGetString("body")))
}

// requestConfig holds per-request parameters, extracted from globals to allow
// future per-request variation (e.g. URL templates, per-scenario methods).
type requestConfig struct {
	method    string
	targetURL string
	retries   int
}

// --- Core logic ---

func doRequest(ctx context.Context, client *http.Client, cfg requestConfig, bodyBytes []byte, hdrs []string, L *lua.LState, captureBody bool) Result {
	var res Result
	var err error
	reqStart := time.Now()

	for attempt := 0; attempt < cfg.retries; attempt++ {
		if ctx.Err() != nil {
			res.Error = ctx.Err()
			res.Latency = time.Since(reqStart)
			return res
		}

		currentHdrs := hdrs
		currentBody := bodyBytes
		if L != nil {
			if fn := L.GetGlobal("pre_request"); fn.Type() == lua.LTFunction {
				rr := buildRequestTable(L, currentHdrs, currentBody)
				if err2 := L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, rr); err2 != nil {
					fmt.Fprintf(os.Stderr, "lua pre_request error: %v\n", err2)
				} else {
					currentHdrs = extractHeaders(rr)
					currentBody = extractBody(rr)
				}
			}
		}

		var bodyReader io.Reader
		if len(currentBody) > 0 {
			bodyReader = bytes.NewReader(currentBody)
		}

		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, cfg.method, cfg.targetURL, bodyReader)
		if err != nil {
			break
		}

		for _, h := range currentHdrs {
			parts := strings.SplitN(h, ":", 2)
			if len(parts) == 2 {
				req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
			}
		}

		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			var respBody []byte
			if captureBody {
				respBody, _ = io.ReadAll(resp.Body)
			} else {
				_, _ = io.ReadAll(resp.Body)
			}
			_ = resp.Body.Close()
			res.Status = resp.StatusCode
			res.Body = respBody

			if L != nil && captureBody {
				if fn := L.GetGlobal("post_response"); fn.Type() == lua.LTFunction {
					rr := buildResponseTable(L, res.Status, res.Body)
					if err2 := L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, rr); err2 != nil {
						fmt.Fprintf(os.Stderr, "lua post_response error: %v\n", err2)
					}
				}
			}
			break
		}

		if attempt+1 < cfg.retries {
			select {
			case <-ctx.Done():
				res.Error = ctx.Err()
				res.Latency = time.Since(reqStart)
				return res
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	res.Latency = time.Since(reqStart)
	res.Error = err
	return res
}

func worker(ctx context.Context, jobs <-chan struct{}, results chan<- Result, client *http.Client, wg *sync.WaitGroup, cfg requestConfig, bodyBytes []byte, hdrs []string, L *lua.LState, captureBody bool) {
	defer wg.Done()
	if L != nil {
		defer L.Close()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-jobs:
			if !ok {
				return
			}
			results <- doRequest(ctx, client, cfg, bodyBytes, hdrs, L, captureBody)
		}
	}
}

// --- Output helpers ---

// pctSorted computes a percentile from an already-sorted slice.
func pctSorted(sorted []float64, p float64) float64 {
	idx := math.Max(0, math.Min(float64(len(sorted)-1), math.Ceil(p/100*float64(len(sorted))))-1)
	return sorted[int(idx)]
}

// pct sorts and computes a percentile (used by final report functions).
func pct(values []float64, p float64) float64 {
	sort.Float64s(values)
	return pctSorted(values, p)
}

func avgFloat(arr []float64) float64 {
	s := 0.0
	for _, v := range arr {
		s += v
	}
	return s / float64(len(arr))
}

func isTTY() bool {
	return term.IsTerminal(os.Stderr.Fd())
}

func printText(latencies []float64, statusCodes map[int]int, errors int, elapsed float64) {
	total := float64(len(latencies))
	sort.Float64s(latencies)

	fmt.Printf("\nTotal time:          %.2f seconds\n", elapsed)
	fmt.Printf("Requests:            %d total, %d failed, %d succeeded\n", len(latencies), errors, len(latencies)-errors)
	fmt.Printf("Requests/sec:        %.2f\n", total/elapsed)
	fmt.Printf("Avg latency:         %.2f ms\n", avgFloat(latencies))
	fmt.Printf("Min latency:         %.2f ms\n", latencies[0])
	fmt.Printf("Max latency:         %.2f ms\n", latencies[len(latencies)-1])
	fmt.Printf("Median (P50):        %.2f ms\n", pct(latencies, 50))
	fmt.Printf("P95:                 %.2f ms\n", pct(latencies, 95))
	fmt.Printf("P99:                 %.2f ms\n", pct(latencies, 99))

	fmt.Println("\nStatus codes:")
	for code, count := range statusCodes {
		fmt.Printf("  %d: %d\n", code, count)
	}
}

func printHistogram(latencies []float64) {
	const numBuckets = 10
	const maxBarWidth = 60

	if len(latencies) < 2 {
		return
	}
	minVal := latencies[0]
	maxVal := latencies[len(latencies)-1]
	if maxVal == minVal {
		fmt.Println("\nHistogram: all requests had identical latency")
		return
	}

	bucketWidth := (maxVal - minVal) / float64(numBuckets)
	counts := make([]int, numBuckets)
	for _, v := range latencies {
		idx := int((v - minVal) / bucketWidth)
		if idx >= numBuckets {
			idx = numBuckets - 1
		}
		counts[idx]++
	}

	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}

	fmt.Println("\nLatency histogram (ms):")
	for i := 0; i < numBuckets; i++ {
		lo := minVal + float64(i)*bucketWidth
		hi := lo + bucketWidth
		barWidth := 0
		if maxCount > 0 {
			barWidth = int(float64(counts[i]) / float64(maxCount) * maxBarWidth)
		}
		bar := strings.Repeat("█", barWidth)
		fmt.Printf("  %7.1f - %7.1f | %-60s %d\n", lo, hi, bar, counts[i])
	}
}

func printJSON(latencies []float64, statusCodes map[int]int, errors int, elapsed float64, durationMode bool) {
	sort.Float64s(latencies)
	total := len(latencies)

	cfg := JSONConfig{
		URL:         targetURL,
		Method:      method,
		Concurrency: concurrency,
		Retries:     retries,
		TimeoutSec:  timeout.Seconds(),
	}
	if durationMode {
		cfg.Duration = duration.String()
	} else {
		cfg.Requests = requests
	}

	scStr := make(map[string]int, len(statusCodes))
	for code, count := range statusCodes {
		scStr[fmt.Sprintf("%d", code)] = count
	}

	out := JSONOutput{
		Config: cfg,
		Summary: JSONSummary{
			TotalRequests:  total,
			Succeeded:      total - errors,
			Failed:         errors,
			TotalTimeSec:   elapsed,
			RequestsPerSec: float64(total) / elapsed,
		},
		StatusCodes: scStr,
		Latency: JSONLatency{
			AvgMs: avgFloat(latencies),
			MinMs: latencies[0],
			MaxMs: latencies[len(latencies)-1],
			P50Ms: pct(latencies, 50),
			P95Ms: pct(latencies, 95),
			P99Ms: pct(latencies, 99),
		},
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

func printCSV(latencies []float64, statusCodes map[int]int, errors int, elapsed float64, durationMode bool) {
	sort.Float64s(latencies)
	total := len(latencies)
	succeeded := total - errors

	w := csv.NewWriter(os.Stdout)
	_ = w.Write([]string{"metric", "value"})

	rows := [][]string{
		{"url", targetURL},
		{"method", method},
		{"concurrency", fmt.Sprintf("%d", concurrency)},
		{"total_time_sec", fmt.Sprintf("%.4f", elapsed)},
		{"requests_per_sec", fmt.Sprintf("%.4f", float64(total)/elapsed)},
		{"total_requests", fmt.Sprintf("%d", total)},
		{"succeeded", fmt.Sprintf("%d", succeeded)},
		{"failed", fmt.Sprintf("%d", errors)},
		{"avg_latency_ms", fmt.Sprintf("%.4f", avgFloat(latencies))},
		{"min_latency_ms", fmt.Sprintf("%.4f", latencies[0])},
		{"max_latency_ms", fmt.Sprintf("%.4f", latencies[total-1])},
		{"p50_latency_ms", fmt.Sprintf("%.4f", pct(latencies, 50))},
		{"p95_latency_ms", fmt.Sprintf("%.4f", pct(latencies, 95))},
		{"p99_latency_ms", fmt.Sprintf("%.4f", pct(latencies, 99))},
	}
	if durationMode {
		rows = append(rows, []string{"duration", duration.String()})
	} else {
		rows = append(rows, []string{"requests_target", fmt.Sprintf("%d", requests)})
	}
	for code, count := range statusCodes {
		rows = append(rows, []string{fmt.Sprintf("status_%d", code), fmt.Sprintf("%d", count)})
	}

	_ = w.WriteAll(rows)
	w.Flush()
}

func checkThresholds(latencies []float64, errCount, total int) bool {
	breached := false

	if thresholdP99 > 0 {
		p99ms := pct(latencies, 99)
		limit := float64(thresholdP99.Milliseconds())
		if p99ms > limit {
			fmt.Fprintf(os.Stderr, "THRESHOLD BREACH: P99 %.1fms > %.0fms\n", p99ms, limit)
			breached = true
		}
	}
	if thresholdP95 > 0 {
		p95ms := pct(latencies, 95)
		limit := float64(thresholdP95.Milliseconds())
		if p95ms > limit {
			fmt.Fprintf(os.Stderr, "THRESHOLD BREACH: P95 %.1fms > %.0fms\n", p95ms, limit)
			breached = true
		}
	}
	if thresholdErr >= 0 && total > 0 {
		actual := float64(errCount) / float64(total) * 100
		if actual > thresholdErr {
			fmt.Fprintf(os.Stderr, "THRESHOLD BREACH: error rate %.1f%% > %.1f%%\n", actual, thresholdErr)
			breached = true
		}
	}
	return breached
}

// --- Main ---

func main() {
	flag.Parse()

	// Interactive wizard: when no -url provided and running in a TTY
	if targetURL == "" && isTTY() {
		result, ok := runWizard()
		if !ok {
			os.Exit(0)
		}
		targetURL = result.url
		method = result.method
		concurrency = result.workers
		requests = result.requests
		duration = result.duration
		timeout = result.timeout
		rateLimit = result.rps
		headers = result.headers
		if result.body != "" {
			bodyStr = result.body
		}
	}

	// Validation
	if targetURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		os.Exit(1)
	}
	if outputFormat != "text" && outputFormat != "json" && outputFormat != "csv" {
		fmt.Fprintln(os.Stderr, "error: -output must be 'text', 'json', or 'csv'")
		os.Exit(1)
	}
	if bodyStr != "" && bodyFile != "" {
		fmt.Fprintln(os.Stderr, "error: -body and -body-file are mutually exclusive")
		os.Exit(1)
	}

	durationMode := duration > 0
	if durationMode && requests != -1 {
		fmt.Fprintln(os.Stderr, "error: -n and -d are mutually exclusive")
		os.Exit(1)
	}
	if !durationMode && requests == -1 {
		requests = 100
	}

	// Load body bytes once
	var bodyBytes []byte
	if bodyFile != "" {
		var err error
		bodyBytes, err = os.ReadFile(bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading body file: %v\n", err)
			os.Exit(1)
		}
	} else if bodyStr != "" {
		bodyBytes = []byte(bodyStr)
	}

	// Load Lua script once, probe for hooks
	var scriptContent string
	var captureBody bool
	if scriptFile != "" {
		data, err := os.ReadFile(scriptFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading script: %v\n", err)
			os.Exit(1)
		}
		scriptContent = string(data)
		probe := lua.NewState()
		defer probe.Close()
		if err := probe.DoString(scriptContent); err != nil {
			fmt.Fprintf(os.Stderr, "error in lua script: %v\n", err)
			os.Exit(1)
		}
		captureBody = probe.GetGlobal("post_response").Type() == lua.LTFunction
	}

	// Context + graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		signal.Stop(sigCh)
		if !isTTY() {
			fmt.Fprintln(os.Stderr, "\nshutdown signal received, stopping...")
		}
		cancel()
	}()

	// Banner (only for non-TUI paths)
	useTUI := isTTY()
	banner := fmt.Sprintf("Sending HTTP %s requests to %s with concurrency %d", method, targetURL, concurrency)
	if durationMode {
		banner = fmt.Sprintf("%s for %s", banner, duration)
	} else {
		banner = fmt.Sprintf("%s (%d requests)", banner, requests)
	}
	if rateLimit > 0 {
		banner = fmt.Sprintf("%s @ %d rps", banner, rateLimit)
	}
	if !useTUI {
		if outputFormat == "json" || outputFormat == "csv" {
			fmt.Fprintln(os.Stderr, banner)
		} else {
			fmt.Println(banner)
		}
	}

	// Channels
	jobBuf := concurrency * 2
	if !durationMode && requests < jobBuf {
		jobBuf = requests
	}
	jobs := make(chan struct{}, jobBuf)

	resultBuf := requests
	if durationMode {
		resultBuf = concurrency * 1000
	}
	results := make(chan Result, resultBuf)

	// HTTP client
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 100,
			MaxConnsPerHost:     100,
		},
	}

	start := time.Now()

	reqCfg := requestConfig{
		method:    method,
		targetURL: targetURL,
		retries:   retries,
	}

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		var L *lua.LState
		if scriptContent != "" {
			L = lua.NewState()
			if err := L.DoString(scriptContent); err != nil {
				fmt.Fprintf(os.Stderr, "lua init error in worker %d: %v\n", i, err)
				os.Exit(1)
			}
		}
		wg.Add(1)
		go worker(ctx, jobs, results, client, &wg, reqCfg, bodyBytes, headers, L, captureBody)
	}

	// Rate limiter
	var tickC <-chan time.Time
	if rateLimit > 0 {
		t := time.NewTicker(time.Second / time.Duration(rateLimit))
		defer t.Stop()
		tickC = t.C
	}

	sendJob := func() bool {
		if rateLimit > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-tickC:
			}
		}
		select {
		case <-ctx.Done():
			return false
		case jobs <- struct{}{}:
			return true
		}
	}

	// Dispatch jobs
	go func() {
		defer close(jobs)
		if durationMode {
			deadline := time.After(duration)
			for {
				select {
				case <-ctx.Done():
					return
				case <-deadline:
					return
				default:
					if !sendJob() {
						return
					}
				}
			}
		} else {
			for j := 0; j < requests; j++ {
				if !sendJob() {
					return
				}
			}
		}
	}()

	// Close results when all workers done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Shared running stats
	rs := newRunningStats(start)

	if useTUI {
		// --- TUI path ---
		cfg := dashConfig{
			method:       method,
			url:          targetURL,
			workers:      concurrency,
			rpsLimit:     rateLimit,
			totalReqs:    requests,
			testDuration: duration,
			durationMode: durationMode,
		}
		p := tea.NewProgram(
			newDashModel(cfg, rs),
			tea.WithOutput(os.Stderr),
			tea.WithAltScreen(),
		)

		go func() {
			for res := range results {
				rs.record(res)
			}
			p.Send(tuiDoneMsg{})
		}()

		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		}
	} else {
		// --- Plain path (non-TTY) ---
		statsCtx, statsCancel := context.WithCancel(ctx)
		var statsWg sync.WaitGroup
		statsWg.Add(1)
		go func() {
			defer statsWg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-statsCtx.Done():
					fmt.Fprint(os.Stderr, "\r\033[K")
					return
				case <-ticker.C:
					completed, lastSnap, errCount, lats := rs.snapshot()
					rs.advanceSnapshot()
					elapsed := time.Since(start).Seconds()
					currentRPS := completed - lastSnap
					var p50, p99 float64
					if len(lats) > 0 {
						p50 = pct(lats, 50)
						p99 = pct(lats, 99)
					}
					errRate := 0.0
					if completed > 0 {
						errRate = float64(errCount) / float64(completed) * 100
					}
					fmt.Fprintf(os.Stderr,
						"\r\033[K[%.0fs] done=%-6d rps=%-5d p50=%.1fms p99=%.1fms err=%.1f%%",
						elapsed, completed, currentRPS, p50, p99, errRate,
					)
				}
			}
		}()

		for res := range results {
			rs.record(res)
		}
		statsCancel()
		statsWg.Wait()
	}

	// Extract final stats from RunningStats
	snap := rs.snapshotFull()
	elapsed := snap.elapsed.Seconds()

	if len(snap.latencies) == 0 {
		fmt.Fprintln(os.Stderr, "No responses received.")
		return
	}

	switch outputFormat {
	case "json":
		printJSON(snap.latencies, snap.statusCodes, snap.errCount, elapsed, durationMode)
	case "csv":
		printCSV(snap.latencies, snap.statusCodes, snap.errCount, elapsed, durationMode)
	default:
		printText(snap.latencies, snap.statusCodes, snap.errCount, elapsed)
		printHistogram(snap.latencies)
	}

	if checkThresholds(snap.latencies, snap.errCount, len(snap.latencies)) {
		os.Exit(1)
	}
}
