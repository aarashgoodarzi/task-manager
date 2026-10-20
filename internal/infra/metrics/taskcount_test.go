package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCounter is a hand-written TaskCounter that can block to expose overlap
// and can fail on demand. entered and release are optional; tests that need
// the periodic behaviour set entered and read it as a refresh signal.
type fakeCounter struct {
	mu          sync.Mutex
	count       int64
	err         error
	calls       int
	inflight    int
	maxInflight int
	entered     chan struct{}
	release     chan struct{}
}

func (f *fakeCounter) CountTasks(ctx context.Context) (int64, error) {
	f.mu.Lock()
	f.calls++
	f.inflight++
	if f.inflight > f.maxInflight {
		f.maxInflight = f.inflight
	}
	entered, release := f.entered, f.release
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count, f.err
}

func (f *fakeCounter) setCount(count int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count = count
}

func (f *fakeCounter) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeCounter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCounter) peakInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflight
}

func newRefresher(t *testing.T, counter TaskCounter, m *Metrics, interval time.Duration) *TaskCountRefresher {
	t.Helper()

	r, err := NewTaskCountRefresher(counter, m, interval, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTaskCountRefresher() unexpected error: %v", err)
	}
	return r
}

func gaugeValue(t *testing.T, m *Metrics, name string) float64 {
	t.Helper()

	family := gatherFamily(t, m, name)
	if len(family.Metric) != 1 {
		t.Fatalf("%s series = %d, want 1", name, len(family.Metric))
	}
	return family.Metric[0].GetGauge().GetValue()
}

func TestRefreshPublishesCountAndTimestamp(t *testing.T) {
	m := New()
	counter := &fakeCounter{count: 3}
	r := newRefresher(t, counter, m, time.Second)

	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() unexpected error: %v", err)
	}

	if got := gaugeValue(t, m, TasksCountName); got != 3 {
		t.Fatalf("%s = %v, want 3", TasksCountName, got)
	}
	if got := gaugeValue(t, m, TasksCountRefreshTimestampName); got <= 0 {
		t.Fatalf("%s = %v, want a Unix timestamp", TasksCountRefreshTimestampName, got)
	}
}

func TestMetricIsAbsentBeforeFirstSuccessfulRefresh(t *testing.T) {
	m := New()

	if family := findFamily(t, m, TasksCountName); family != nil {
		t.Fatalf("%s is exposed before any successful refresh: %v", TasksCountName, family)
	}
	if family := findFamily(t, m, TasksCountRefreshTimestampName); family != nil {
		t.Fatalf("%s is exposed before any successful refresh: %v", TasksCountRefreshTimestampName, family)
	}
}

func TestRefreshFailureKeepsLastKnownValue(t *testing.T) {
	m := New()
	counter := &fakeCounter{count: 4}
	r := newRefresher(t, counter, m, time.Second)

	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() unexpected error: %v", err)
	}
	timestampBefore := gaugeValue(t, m, TasksCountRefreshTimestampName)

	dbErr := errors.New("database unreachable")
	counter.setError(dbErr)

	err := r.refresh(context.Background())
	if !errors.Is(err, dbErr) {
		t.Fatalf("refresh() error = %v, want the database error", err)
	}
	if got := gaugeValue(t, m, TasksCountName); got != 4 {
		t.Fatalf("%s = %v after a failed refresh, want the last known 4 (not zero)", TasksCountName, got)
	}
	if got := gaugeValue(t, m, TasksCountRefreshTimestampName); got != timestampBefore {
		t.Fatalf("%s advanced after a failed refresh: %v -> %v", TasksCountRefreshTimestampName, timestampBefore, got)
	}
}

func TestRefreshFailureBeforeFirstSuccessLeavesMetricAbsent(t *testing.T) {
	m := New()
	counter := &fakeCounter{err: errors.New("database unreachable")}
	r := newRefresher(t, counter, m, time.Second)

	if err := r.refresh(context.Background()); err == nil {
		t.Fatal("refresh() want an error, got nil")
	}
	if family := findFamily(t, m, TasksCountName); family != nil {
		t.Fatalf("%s is exposed after only failed refreshes: %v", TasksCountName, family)
	}
}

func TestNegativeCountIsRejected(t *testing.T) {
	m := New()
	counter := &fakeCounter{count: -1}
	r := newRefresher(t, counter, m, time.Second)

	if err := r.refresh(context.Background()); err == nil {
		t.Fatal("refresh() with a negative count: want error, got nil")
	}
	if family := findFamily(t, m, TasksCountName); family != nil {
		t.Fatalf("%s is exposed after an invalid count: %v", TasksCountName, family)
	}
}

func TestRefreshAndLogReportsFailureWithoutPublishingZero(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	m := New()
	counter := &fakeCounter{err: errors.New("database unreachable")}
	r, err := NewTaskCountRefresher(counter, m, time.Second, log)
	if err != nil {
		t.Fatalf("NewTaskCountRefresher() unexpected error: %v", err)
	}

	r.refreshAndLog(context.Background())

	if !strings.Contains(buf.String(), "task count refresh failed") {
		t.Fatalf("log = %q, want a refresh failure message", buf.String())
	}
	if family := findFamily(t, m, TasksCountName); family != nil {
		t.Fatalf("%s is exposed after a failed refresh: %v", TasksCountName, family)
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	m := New()
	counter := &fakeCounter{count: 1, entered: make(chan struct{}, 1)}
	r := newRefresher(t, counter, m, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	select {
	case <-counter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not perform the initial refresh")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestRunRefreshesOnItsInterval(t *testing.T) {
	m := New()
	counter := &fakeCounter{count: 2, entered: make(chan struct{}, 8)}
	r := newRefresher(t, counter, m, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The initial refresh plus two ticks prove the loop keeps running. Reads are
	// bounded by a timeout rather than an unconditional sleep.
	for i := 0; i < 3; i++ {
		select {
		case <-counter.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("refresh %d did not happen", i+1)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestConcurrentRefreshesAreSerialized(t *testing.T) {
	m := New()
	counter := &fakeCounter{
		count:   7,
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	r := newRefresher(t, counter, m, time.Second)

	go func() { _ = r.refresh(context.Background()) }()
	select {
	case <-counter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh did not start")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_ = r.refresh(context.Background())
	}()

	select {
	case <-counter.entered:
		t.Fatal("a second refresh entered the database while the first was still running")
	case <-time.After(100 * time.Millisecond):
		// Expected: the second refresh waits on refreshMu.
	}

	close(counter.release)
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second refresh did not complete after the first was released")
	}

	if got := counter.peakInflight(); got != 1 {
		t.Fatalf("peak concurrent counts = %d, want 1", got)
	}
	if got := counter.callCount(); got != 2 {
		t.Fatalf("count calls = %d, want 2", got)
	}
}

func TestNewTaskCountRefresherValidatesInputs(t *testing.T) {
	m := New()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	tests := []struct {
		name     string
		counter  TaskCounter
		metrics  *Metrics
		interval time.Duration
	}{
		{name: "missing counter", metrics: m, interval: time.Second},
		{name: "missing metrics", counter: &fakeCounter{}, interval: time.Second},
		{name: "zero interval", counter: &fakeCounter{}, metrics: m},
		{name: "negative interval", counter: &fakeCounter{}, metrics: m, interval: -time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewTaskCountRefresher(tc.counter, tc.metrics, tc.interval, log); err == nil {
				t.Fatal("NewTaskCountRefresher() want error, got nil")
			}
		})
	}
}
