package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reportMetrics captures AWS-style REPORT line fields emitted by the RIE.
type reportMetrics struct {
	RequestID        string
	DurationMs       float64
	BilledDurationMs float64
}

// invocationMetrics mirrors the structured JSON log we emit from the event loop.
type invocationMetrics struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`

	RequestID   string `json:"request_id"`
	Outcome     string `json:"outcome"`
	ErrorType   string `json:"error_type"`
	NextMs      int64  `json:"next_ms"`
	UnmarshalMs int64  `json:"unmarshal_ms"`
	ValidateMs  int64  `json:"validate_ms"`
	HandlerMs   int64  `json:"handler_ms"`
	MarshalMs   int64  `json:"marshal_ms"`
	PostMs      int64  `json:"post_ms"`
	TotalMs     int64  `json:"total_ms"`
}

// waitForPort pings a TCP port until it's accepting connections or times out.
func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s: %w", addr, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// runCmd runs an external command and returns stdout as string.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%s %v failed: %w: %s", name, args, err, stderr.String())
		}
		return "", fmt.Errorf("%s %v failed: %w", name, args, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// parseReportLines extracts RequestId, Duration and Billed Duration from RIE logs.
func parseReportLines(logText string) map[string]reportMetrics {
	// Handles variants with tabs or spaces and ignores optional "Init Duration"
	out := make(map[string]reportMetrics)
	scanner := bufio.NewScanner(strings.NewReader(logText))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "REPORT RequestId:") {
			continue
		}

		// Extract request id robustly
		rid := ""
		reRID := regexp.MustCompile(`REPORT\s+RequestId:\s*([a-zA-Z0-9-]+)`) // covers both tab and space separated
		if m := reRID.FindStringSubmatch(line); len(m) == 2 {
			rid = strings.TrimSpace(m[1])
		}
		if rid == "" {
			// Best-effort tab-split as a secondary path
			tokens := strings.Split(line, "\t")
			if len(tokens) > 0 {
				const pfx = "REPORT RequestId: "
				first := strings.TrimSpace(tokens[0])
				if strings.HasPrefix(first, pfx) {
					rid = strings.TrimSpace(strings.TrimPrefix(first, pfx))
				}
			}
		}
		if rid == "" {
			continue
		}

		// Remove optional Init Duration to avoid confusing the Duration regex
		reInit := regexp.MustCompile(`Init\s+Duration:\s*[0-9.]+\s*ms`)
		clean := reInit.ReplaceAllString(line, "")

		// Extract Duration and Billed Duration tolerant of spaces or tabs
		reDur := regexp.MustCompile(`\bDuration:\s*([0-9.]+)\s*ms`)
		reBill := regexp.MustCompile(`\bBilled\s+Duration:\s*([0-9.]+)\s*ms`)
		var d, b float64
		if m := reDur.FindStringSubmatch(clean); len(m) == 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				d = v
			}
		}
		if m := reBill.FindStringSubmatch(clean); len(m) == 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				b = v
			}
		}

		out[rid] = reportMetrics{RequestID: rid, DurationMs: d, BilledDurationMs: b}
	}
	return out
}

// parseInvocationMetrics scans JSON lines with message=="invocation.metrics".
func parseInvocationMetrics(logText string) map[string]invocationMetrics {
	out := make(map[string]invocationMetrics)
	scanner := bufio.NewScanner(strings.NewReader(logText))
	// Increase buffer for large JSON lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "\"invocation.metrics\"") {
			continue
		}
		var m invocationMetrics
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.Message == "invocation.metrics" && m.RequestID != "" {
			out[m.RequestID] = m
		}
	}
	return out
}

// invokeOnce posts a simple JSON payload to the RIE endpoint and returns the round-trip duration.
func invokeOnce(client *http.Client, payload string) (time.Duration, error) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:9000/2015-03-31/functions/function/invocations", strings.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Check HTTP status and drain response fully
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.ReadAll(resp.Body)
	return time.Since(start), nil
}

// BenchmarkRIEEndToEnd builds the image, runs the RIE container, invokes N times,
// collects logs, and compares billed duration vs internal timings from the event loop.
func BenchmarkRIEEndToEnd(b *testing.B) {
	// Keep setup cost out of the measured iterations
	b.StopTimer()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Build image (Dockerfile is at repo root). The test runs in bench/.
	if _, err := runCmd(ctx, "podman", "build", "-t", "testlambda", "-f", "../Dockerfile", ".."); err != nil {
		b.Fatalf("podman build failed: %v", err)
	}

	// Run container with RIE as entrypoint
	containerID, err := runCmd(ctx, "podman", "run", "-d", "-p", "9000:8080", "--entrypoint", "/usr/local/bin/aws-lambda-rie", "testlambda:latest", "./bootstrap")
	if err != nil {
		b.Fatalf("podman run failed: %v", err)
	}
	defer func() {
		_, _ = runCmd(context.Background(), "podman", "rm", "-f", containerID)
	}()

	if err := waitForPort("127.0.0.1:9000", 20*time.Second); err != nil {
		b.Fatalf("RIE did not become ready: %v", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}

	payload := `{"input":"test"}`

	// Warm-up one invocation to avoid cold-start skew in benchmark loop
	if _, err := invokeOnce(client, payload); err != nil {
		b.Fatalf("warm-up invoke failed: %v", err)
	}

	// Mark the beginning of the measured window for log filtering
	sinceTS := time.Now()

	// Measured invocations
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if _, err := invokeOnce(client, payload); err != nil {
			b.Fatalf("invoke failed: %v", err)
		}
	}
	b.StopTimer()

	// Wait briefly until at least b.N REPORT lines appear since sinceTS
	deadline := time.Now().Add(5 * time.Second)
	for {
		logsTry, _ := runCmd(ctx, "podman", "logs", "--since", sinceTS.Format(time.RFC3339Nano), containerID)
		if len(parseReportLines(logsTry)) >= b.N {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Collect container logs since the measured window
	logs, err := runCmd(ctx, "podman", "logs", "--since", sinceTS.Format(time.RFC3339Nano), containerID)
	if err != nil {
		b.Fatalf("podman logs failed: %v", err)
	}

	// Parse billed and internal metrics
	billed := parseReportLines(logs)
	internal := parseInvocationMetrics(logs)

	// Join on request id and compute aggregates
	type pair struct {
		billed   reportMetrics
		internal invocationMetrics
	}
	pairs := make([]pair, 0, len(internal))
	for rid, im := range internal {
		if rm, ok := billed[rid]; ok {
			pairs = append(pairs, pair{billed: rm, internal: im})
		}
	}

	if len(pairs) == 0 {
		// Degrade gracefully when internal metrics are not emitted
		if len(internal) == 0 && len(billed) > 0 {
			var sumBilled float64
			for _, rm := range billed {
				sumBilled += rm.BilledDurationMs
			}
			avgBilled := sumBilled / float64(len(billed))
			b.ReportMetric(avgBilled, "ms/billed_avg")
			b.Logf("no internal metrics found; reporting billed_avg only over %d invocations", len(billed))
			return
		}
		b.Fatalf("no matched request IDs between billed and internal metrics; ensure RIE emits REPORT lines and event loop logs invocation.metrics")
	}

	var (
		sumBilled   float64
		sumInternal float64
		maxGap      float64
		minGap      = 1e9
	)
	for _, p := range pairs {
		sumBilled += p.billed.BilledDurationMs
		sumInternal += float64(p.internal.TotalMs)
		gap := p.billed.BilledDurationMs - float64(p.internal.TotalMs)
		if gap > maxGap {
			maxGap = gap
		}
		if gap < minGap {
			minGap = gap
		}
	}

	avgBilled := sumBilled / float64(len(pairs))
	avgInternal := sumInternal / float64(len(pairs))
	avgGap := avgBilled - avgInternal

	// Report as benchmark metrics
	b.ReportMetric(avgBilled, "ms/billed_avg")
	b.ReportMetric(avgInternal, "ms/internal_avg")
	b.ReportMetric(avgGap, "ms/gap_avg")
	b.ReportMetric(maxGap, "ms/gap_max")
	b.ReportMetric(minGap, "ms/gap_min")

	// Emit a concise comparison table via logs
	b.Logf("pairs=%d avg_billed=%.2fms avg_internal=%.2fms gap_avg=%.2fms gap_min=%.2fms gap_max=%.2fms", len(pairs), avgBilled, avgInternal, avgGap, minGap, maxGap)
}

// BenchmarkRIEHeavy simulates heavier workloads to surface per-phase bottlenecks.
func BenchmarkRIEHeavy(b *testing.B) {
	b.StopTimer()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// Build image (Dockerfile is at repo root). The test runs in bench/.
	if _, err := runCmd(ctx, "podman", "build", "-t", "testlambda", "-f", "../Dockerfile", ".."); err != nil {
		b.Fatalf("podman build failed: %v", err)
	}

	// Run container with RIE as entrypoint
	containerID, err := runCmd(ctx, "podman", "run", "-d", "-p", "9000:8080", "--entrypoint", "/usr/local/bin/aws-lambda-rie", "testlambda:latest", "./bootstrap")
	if err != nil {
		b.Fatalf("podman run failed: %v", err)
	}
	defer func() { _, _ = runCmd(context.Background(), "podman", "rm", "-f", containerID) }()

	if err := waitForPort("127.0.0.1:9000", 20*time.Second); err != nil {
		b.Fatalf("RIE did not become ready: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	// Use a mix of sleep-bound and cpu-bound loads
	payloads := []string{
		`{"input":"sleep:20ms"}`,
		`{"input":"sleep:50ms"}`,
		`{"input":"cpu:20ms"}`,
		`{"input":"cpu:50ms"}`,
	}

	// Warm-up across all payloads
	for _, p := range payloads {
		if _, err := invokeOnce(client, p); err != nil {
			b.Fatalf("warm-up invoke failed: %v", err)
		}
	}

	// Mark the beginning of the measured window for log filtering
	sinceTS := time.Now()

	// Run N invocations (payload chosen round-robin)
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		p := payloads[i%len(payloads)]
		if _, err := invokeOnce(client, p); err != nil {
			b.Fatalf("invoke failed: %v", err)
		}
	}
	b.StopTimer()

	// Wait briefly until at least b.N REPORT lines appear since sinceTS
	deadline := time.Now().Add(5 * time.Second)
	for {
		logsTry, _ := runCmd(ctx, "podman", "logs", "--since", sinceTS.Format(time.RFC3339Nano), containerID)
		if len(parseReportLines(logsTry)) >= b.N {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Collect and parse logs since the measured window
	logs, err := runCmd(ctx, "podman", "logs", "--since", sinceTS.Format(time.RFC3339Nano), containerID)
	if err != nil {
		b.Fatalf("podman logs failed: %v", err)
	}
	internal := parseInvocationMetrics(logs)
	billed := parseReportLines(logs)

	// Aggregate per-phase timings for matched pairs
	type agg struct {
		count                                                    int
		next, unmarshal, validate, handler, marshal, post, total float64
		billed                                                   float64
	}
	// If no internal metrics are present, report billed only and return
	if len(internal) == 0 && len(billed) > 0 {
		var sum float64
		for _, rm := range billed {
			sum += rm.BilledDurationMs
		}
		avg := sum / float64(len(billed))
		b.ReportMetric(avg, "ms/billed_avg")
		b.Logf("no internal metrics found in heavy benchmark; reporting billed_avg only over %d invocations", len(billed))
		return
	}

	a := agg{}
	for rid, im := range internal {
		if rm, ok := billed[rid]; ok {
			a.count++
			a.next += float64(im.NextMs)
			a.unmarshal += float64(im.UnmarshalMs)
			a.validate += float64(im.ValidateMs)
			a.handler += float64(im.HandlerMs)
			a.marshal += float64(im.MarshalMs)
			a.post += float64(im.PostMs)
			a.total += float64(im.TotalMs)
			a.billed += rm.BilledDurationMs
		}
	}
	if a.count == 0 {
		// If internal existed but didn't match billed, still try to report billed-only
		if len(billed) > 0 {
			var sum float64
			for _, rm := range billed {
				sum += rm.BilledDurationMs
			}
			avg := sum / float64(len(billed))
			b.ReportMetric(avg, "ms/billed_avg")
			b.Logf("no matched request IDs; reporting billed_avg only over %d invocations", len(billed))
			return
		}
		b.Fatalf("no matched request IDs between billed and internal metrics in heavy test")
	}

	// Report percentages of total internal time per phase
	var nextPct, unmarshalPct, validatePct, handlerPct, marshalPct, postPct float64
	if a.total > 0 {
		nextPct = (a.next / a.total) * 100
		unmarshalPct = (a.unmarshal / a.total) * 100
		validatePct = (a.validate / a.total) * 100
		handlerPct = (a.handler / a.total) * 100
		marshalPct = (a.marshal / a.total) * 100
		postPct = (a.post / a.total) * 100
	}
	b.ReportMetric(nextPct, "pct/next_share")
	b.ReportMetric(unmarshalPct, "pct/unmarshal_share")
	b.ReportMetric(validatePct, "pct/validate_share")
	b.ReportMetric(handlerPct, "pct/handler_share")
	b.ReportMetric(marshalPct, "pct/marshal_share")
	b.ReportMetric(postPct, "pct/post_share")
	// Keep billed average for reference
	b.ReportMetric(a.billed/float64(a.count), "ms/billed_avg")

	b.Logf("heavy_invocations=%d share_next=%.2f%% share_unmarshal=%.2f%% share_validate=%.2f%% share_handler=%.2f%% share_marshal=%.2f%% share_post=%.2f%% billed_avg=%.2fms",
		a.count, nextPct, unmarshalPct, validatePct, handlerPct, marshalPct, postPct, a.billed/float64(a.count))
}
