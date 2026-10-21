package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	goredis "github.com/redis/go-redis/v9"
)

func TestPercentileUsesNearestRank(t *testing.T) {
	var samples []time.Duration
	for i := 1; i <= 100; i++ {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{50, 50 * time.Millisecond},
		{95, 95 * time.Millisecond},
		{99, 99 * time.Millisecond},
		{100, 100 * time.Millisecond},
		{0, 1 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := percentile(samples, tc.p); got != tc.want {
			t.Errorf("percentile(p=%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := percentile(nil, 95); got != 0 {
		t.Errorf("percentile of empty sample = %v, want 0", got)
	}
}

func TestSummarizeExcludesFailuresFromLatency(t *testing.T) {
	latencies := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	s := summarize(latencies, time.Second, 2)

	if s.Requests != 4 || s.Successful != 2 || s.Failed != 2 {
		t.Fatalf("counts = %d/%d/%d, want 4 total / 2 successful / 2 failed", s.Requests, s.Successful, s.Failed)
	}
	if s.RPS < 3.9 || s.RPS > 4.1 {
		t.Fatalf("RPS = %v, want ~4", s.RPS)
	}
	if s.Mean != 15*time.Millisecond {
		t.Fatalf("Mean = %v, want 15ms", s.Mean)
	}
	if s.P99 != 20*time.Millisecond || s.Max != 20*time.Millisecond {
		t.Fatalf("P99/Max = %v/%v, want 20ms", s.P99, s.Max)
	}
}

func TestSummarizeWithNoSuccesses(t *testing.T) {
	s := summarize(nil, time.Second, 5)
	if s.Requests != 5 || s.Successful != 0 || s.Failed != 5 {
		t.Fatalf("counts = %d/%d/%d", s.Requests, s.Successful, s.Failed)
	}
	if s.Mean != 0 || s.P50 != 0 || s.Max != 0 {
		t.Fatalf("latency fields = %v/%v/%v, want zero", s.Mean, s.P50, s.Max)
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(fmt.Errorf("wrapped: %w", errUsage)); got != 2 {
		t.Errorf("exitCode(usage) = %d, want 2", got)
	}
	if got := exitCode(fmt.Errorf("wrapped: %w", errFailedRequests)); got != 1 {
		t.Errorf("exitCode(failures) = %d, want 1", got)
	}
	if got := exitCode(fmt.Errorf("wrapped: %w", errEvidenceFailed)); got != 1 {
		t.Errorf("exitCode(evidence) = %d, want 1", got)
	}
}

func TestEvaluateCacheEvidenceWarm(t *testing.T) {
	ok := evidenceDelta{hits: 100, misses: 0, variantDelta: 0, afterKeys: 1}
	if err := evaluateCacheEvidence(modeWarm, 50, ok); err != nil {
		t.Fatalf("warm OK evidence: %v", err)
	}
	cases := []struct {
		name string
		d    evidenceDelta
	}{
		{"misses", evidenceDelta{hits: 10, misses: 1, variantDelta: 0, afterKeys: 1}},
		{"new keys", evidenceDelta{hits: 10, misses: 0, variantDelta: 2, afterKeys: 3}},
		{"no keys", evidenceDelta{hits: 10, misses: 0, variantDelta: 0, afterKeys: 0}},
	}
	for _, tc := range cases {
		err := evaluateCacheEvidence(modeWarm, 50, tc.d)
		if !errors.Is(err, errEvidenceFailed) {
			t.Errorf("%s: err = %v, want errEvidenceFailed", tc.name, err)
		}
	}
}

func TestEvaluateCacheEvidenceMiss(t *testing.T) {
	ok := evidenceDelta{hits: 10, misses: 10, variantDelta: 10, afterKeys: 11}
	if err := evaluateCacheEvidence(modeMiss, 10, ok); err != nil {
		t.Fatalf("miss OK evidence: %v", err)
	}
	if err := evaluateCacheEvidence(modeMiss, 10, evidenceDelta{hits: 10, misses: 9, variantDelta: 10, afterKeys: 11}); !errors.Is(err, errEvidenceFailed) {
		t.Fatalf("too few misses: err = %v", err)
	}
	if err := evaluateCacheEvidence(modeMiss, 10, evidenceDelta{hits: 10, misses: 10, variantDelta: 9, afterKeys: 10}); !errors.Is(err, errEvidenceFailed) {
		t.Fatalf("too few keys: err = %v", err)
	}
}

func TestParseFlagsDefaultsAndValidation(t *testing.T) {
	o, err := parseFlags(nil, &strings.Builder{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.baseURL != "http://127.0.0.1:8080" || o.requests != 1000 || o.concurrency != 8 || o.mode != modeWarm {
		t.Fatalf("defaults = %+v", o)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"miss without redis", []string{"-mode", "miss"}},
		{"miss with concurrency", []string{"-mode", "miss", "-redis-url", "redis://localhost:6379/0", "-concurrency", "2"}},
		{"bad mode", []string{"-mode", "hot"}},
		{"zero requests", []string{"-requests", "0"}},
		{"negative concurrency", []string{"-concurrency", "-1"}},
		{"zero timeout", []string{"-timeout", "0s"}},
		{"remote seed", []string{"-base-url", "http://192.0.2.10:8080", "-seed", "10"}},
		{"fragment in base url", []string{"-base-url", "http://127.0.0.1:8080/#x"}},
		{"path in base url", []string{"-base-url", "http://127.0.0.1:8080/api"}},
		{"stray argument", []string{"extra"}},
		{"unknown flag", []string{"-nope"}},
	}
	for _, tc := range cases {
		if _, err := parseFlags(tc.args, &strings.Builder{}); err == nil {
			t.Errorf("%s: parseFlags succeeded, want an error", tc.name)
		}
	}
}

func TestParseFlagsRejectsBaseURLWithPathAsUsage(t *testing.T) {
	_, err := parseFlags([]string{"-base-url", "http://127.0.0.1:8080/api"}, &strings.Builder{})
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want errUsage", err)
	}
	if got := exitCode(err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

func TestExitCodeForUnknownFlag(t *testing.T) {
	_, err := parseFlags([]string{"-nope"}, &strings.Builder{})
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if got := exitCode(err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

func TestRunWarmAgainstTestServer(t *testing.T) {
	var hits int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"page":1,"page_size":20,"total":0,"total_pages":0}`))
	}))
	defer srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL:     srv.URL,
		query:       "page=1&page_size=20",
		requests:    50,
		concurrency: 4,
		timeout:     2 * time.Second,
		warmup:      5,
		mode:        modeWarm,
		redisPrefix: "task-manager",
	}, out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 55 {
		t.Fatalf("server received %d requests, want 50 measured + 5 warmup", got)
	}
	for _, want := range []string{"successful   50", "failed       0", "p50", "p95", "p99", "throughput"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunReportsFailedStatusAndFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL: srv.URL, query: "page=1", requests: 10, concurrency: 2,
		timeout: time.Second, mode: modeWarm, redisPrefix: "task-manager",
	}, out)
	if !errors.Is(err, errFailedRequests) {
		t.Fatalf("run error = %v, want errFailedRequests\n%s", err, out)
	}
	if !strings.Contains(out.String(), "failed       10") {
		t.Fatalf("output did not report 10 failures:\n%s", out)
	}
	if !strings.Contains(out.String(), "unexpected status 500") {
		t.Fatalf("output did not sample the status failure:\n%s", out)
	}
}

func TestRunCountsTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL: deadURL, query: "page=1", requests: 3, concurrency: 1,
		timeout: 500 * time.Millisecond, mode: modeWarm, redisPrefix: "task-manager",
	}, out)
	if !errors.Is(err, errFailedRequests) {
		t.Fatalf("run error = %v, want errFailedRequests\n%s", err, out)
	}
	if !strings.Contains(out.String(), "successful   0") {
		t.Fatalf("output did not report zero successes:\n%s", out)
	}
}

func TestRunRequestTimeoutIsCountedAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL: srv.URL, query: "page=1", requests: 2, concurrency: 1,
		timeout: 50 * time.Millisecond, mode: modeWarm, redisPrefix: "task-manager",
	}, out)
	if !errors.Is(err, errFailedRequests) {
		t.Fatalf("run error = %v, want errFailedRequests\n%s", err, out)
	}
	if !strings.Contains(out.String(), "failed       2") {
		t.Fatalf("output did not report both timeouts:\n%s", out)
	}
}

// fakeAPI is a minimal in-memory stand-in for the task service used to test
// dataset preparation without a database.
type fakeAPI struct {
	mu       sync.Mutex
	nextID   int
	tasks    map[string]string
	created  []string
	deleted  []string
	seedSize int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{tasks: map[string]string{}}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/task":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.nextID++
		id := "id-" + strconv.Itoa(f.nextID)
		f.tasks[id] = body["title"]
		f.created = append(f.created, id)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "title": body["title"]})
	case r.Method == http.MethodGet && r.URL.Path == "/tasks":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[],"page":1,"page_size":1,"total":%d,"total_pages":%d}`, len(f.tasks), len(f.tasks))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/tasks/"):
		id := strings.TrimPrefix(r.URL.Path, "/tasks/")
		if _, ok := f.tasks[id]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.tasks, id)
		f.deleted = append(f.deleted, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func TestRunSeedsKnownDatasetAndCleansItUp(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL: srv.URL, query: "page=1&page_size=20", requests: 5, concurrency: 2,
		timeout: 2 * time.Second, mode: modeWarm, redisPrefix: "task-manager", seed: 3,
	}, out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	api.mu.Lock()
	created, deleted, remaining := len(api.created), len(api.deleted), len(api.tasks)
	api.mu.Unlock()

	if created != 3 {
		t.Fatalf("created %d tasks, want 3", created)
	}
	if deleted != 3 || remaining != 0 {
		t.Fatalf("deleted %d, remaining %d, want 3 deleted and 0 remaining", deleted, remaining)
	}
	for _, want := range []string{"seeded       3 tasks", "total        0 -> 3", "deleted      3 of 3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunKeepLeavesDatasetAndReportsIt(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL: srv.URL, query: "page=1", requests: 2, concurrency: 1,
		timeout: 2 * time.Second, mode: modeWarm, redisPrefix: "task-manager", seed: 2, keep: true,
	}, out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	api.mu.Lock()
	remaining := len(api.tasks)
	api.mu.Unlock()
	if remaining != 2 {
		t.Fatalf("remaining tasks = %d, want 2 with -keep", remaining)
	}
	if !strings.Contains(out.String(), "cleanup skipped (-keep)") {
		t.Fatalf("output did not report skipped cleanup:\n%s", out)
	}
}

func TestRunWarmupFailureAbortsBeforeMeasurement(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 3 {
			http.Error(w, "warmup boom", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"page":1,"page_size":20,"total":0,"total_pages":0}`))
	}))
	defer srv.Close()

	out := &strings.Builder{}
	err := run(context.Background(), options{
		baseURL: srv.URL, query: "page=1", requests: 10, concurrency: 1,
		timeout: time.Second, warmup: 3, mode: modeWarm, redisPrefix: "task-manager",
	}, out)
	if !errors.Is(err, errFailedRequests) {
		t.Fatalf("run error = %v, want errFailedRequests\n%s", err, out)
	}
	if !strings.Contains(err.Error(), "warmup") {
		t.Fatalf("error = %v, want a warmup failure", err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server received %d requests, want only the 3 warmup attempts", got)
	}
}

func TestMissModeReplaceGenerationUpdatesRedisKey(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set; skipping miss-mode Redis generation test")
	}

	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })

	prefix := fmt.Sprintf("loadtest-miss-%d", time.Now().UnixNano())
	genKey := prefix + ":tasks:list:generation"
	ctx := context.Background()
	if err := client.Set(ctx, genKey, "before-token", 0).Err(); err != nil {
		t.Fatalf("seed generation key: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(ctx, genKey).Err() })

	r := &runner{
		mode:   modeMiss,
		redis:  client,
		genKey: genKey,
	}
	if err := r.replaceGeneration(ctx); err != nil {
		t.Fatalf("replaceGeneration: %v", err)
	}
	got, err := client.Get(ctx, genKey).Result()
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}
	if got == "" || got == "before-token" {
		t.Fatalf("generation = %q, want a fresh token replacing before-token", got)
	}
	if len(got) != 32 {
		t.Fatalf("generation length = %d, want 32 hex chars", len(got))
	}
}
