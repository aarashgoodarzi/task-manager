package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TaskCounter is the persistence capability the refresher needs. It is
// declared here, at the point of use, so this package does not depend on the
// PostgreSQL repository or on any SQL driver. The repository's CountTasks
// method satisfies it.
type TaskCounter interface {
	CountTasks(ctx context.Context) (int64, error)
}

// TaskCountRefresher periodically republishes the persisted task count as the
// tasks_count gauge.
//
// The count is read from PostgreSQL on a fixed interval rather than on every
// HTTP request: a full-table count per request would put avoidable load on the
// database for a value that changes only with task writes. The price is
// eventual consistency, bounded by the refresh interval plus the duration of
// one count query.
//
// A failed refresh never publishes a fabricated value. The last successful
// count remains visible (it is stale, not wrong) and
// tasks_count_last_refresh_timestamp_seconds stops advancing; before the first
// successful count, tasks_count is absent from the exposition entirely rather
// than reporting zero.
type TaskCountRefresher struct {
	counter  TaskCounter
	metric   *taskCountCollector
	interval time.Duration
	log      *slog.Logger

	// refreshMu serializes refreshes. Run is a single goroutine, but the lock
	// makes the no-overlap guarantee hold even if Refresh is triggered from
	// elsewhere later.
	refreshMu sync.Mutex
}

// NewTaskCountRefresher validates its inputs and returns a refresher. interval
// must be positive; the composition root falls back to the configured default
// before calling this.
func NewTaskCountRefresher(counter TaskCounter, m *Metrics, interval time.Duration, log *slog.Logger) (*TaskCountRefresher, error) {
	if counter == nil {
		return nil, fmt.Errorf("metrics: task count refresher: counter is required")
	}
	if m == nil {
		return nil, fmt.Errorf("metrics: task count refresher: metrics are required")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("metrics: task count refresher: interval must be positive, got %s", interval)
	}

	return &TaskCountRefresher{
		counter:  counter,
		metric:   m.tasksCount,
		interval: interval,
		log:      log,
	}, nil
}

// Run refreshes immediately and then on every tick until ctx is cancelled. It
// blocks, so the caller owns the goroutine and must wait for it before closing
// the database pool.
//
// A refresh never overlaps the next one: the ticker is consumed by this loop
// and refresh holds refreshMu, so a slow query delays the next tick instead of
// stacking a second query on top of it.
func (r *TaskCountRefresher) Run(ctx context.Context) {
	r.refreshAndLog(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshAndLog(ctx)
		}
	}
}

// refreshAndLog records a refresh failure without turning it into a metric
// value. Cancellation during shutdown is expected and is not logged as a
// failure.
func (r *TaskCountRefresher) refreshAndLog(ctx context.Context) {
	if err := r.refresh(ctx); err != nil && ctx.Err() == nil {
		if r.log != nil {
			r.log.Warn("task count refresh failed; keeping the last known value",
				slog.Any("error", err),
			)
		}
	}
}

// refresh reads the current count and publishes it. It returns the error
// unchanged (wrapped) so the caller can log or test it; the collector is only
// updated on success.
func (r *TaskCountRefresher) refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	count, err := r.counter.CountTasks(ctx)
	if err != nil {
		return fmt.Errorf("metrics: count tasks: %w", err)
	}
	if count < 0 {
		return fmt.Errorf("metrics: count tasks: negative count %d", count)
	}

	r.metric.set(count, time.Now())
	return nil
}

// taskCountCollector publishes tasks_count and its refresh timestamp. It
// implements prometheus.Collector instead of using a prometheus.Gauge so that
// "no successful count yet" is representable as an absent series, not as a
// plausible-looking zero.
type taskCountCollector struct {
	countDesc     *prometheus.Desc
	timestampDesc *prometheus.Desc

	mu          sync.RWMutex
	count       int64
	refreshedAt time.Time
	valid       bool
}

func newTaskCountCollector() *taskCountCollector {
	return &taskCountCollector{
		countDesc: prometheus.NewDesc(
			TasksCountName,
			"Current number of tasks persisted in PostgreSQL.",
			nil, nil,
		),
		timestampDesc: prometheus.NewDesc(
			TasksCountRefreshTimestampName,
			"Unix timestamp of the last successful tasks_count refresh.",
			nil, nil,
		),
	}
}

// Describe sends both descriptors unconditionally, so the registry knows the
// metric types even before the first successful refresh.
func (c *taskCountCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.countDesc
	ch <- c.timestampDesc
}

// Collect emits nothing until a count has succeeded. After that it emits the
// last known count and the time it was observed, which makes staleness visible
// without pretending a failed refresh succeeded.
func (c *taskCountCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.valid {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.GaugeValue, float64(c.count))
	ch <- prometheus.MustNewConstMetric(c.timestampDesc, prometheus.GaugeValue, float64(c.refreshedAt.Unix()))
}

func (c *taskCountCollector) set(count int64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.count = count
	c.refreshedAt = at
	c.valid = true
}
