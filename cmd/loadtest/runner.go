package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// runner executes one scenario. Every field is fixed before the run; the
// runner creates exactly -concurrency goroutines and never more.
type runner struct {
	client *http.Client
	target string
	mode   string

	requests    int
	concurrency int

	// redis is set only when the operator supplied -redis-url. In miss mode it
	// is required; in warm mode it is used to report cache evidence.
	redis     *goredis.Client
	genKey    string
	listMatch string
}

// result is one measured phase. Latency samples belong to successful requests
// only; failures are counted and their latency is deliberately excluded so a
// fast rejection cannot flatter the percentiles.
type result struct {
	latencies []time.Duration
	failures  int64
	wall      time.Duration
	samples   []string
}

// newRunner builds the worker pool description and the exact URL under test.
func newRunner(o options, client *http.Client, base *url.URL) (*runner, error) {
	query := strings.TrimPrefix(o.query, "?")
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + "/tasks"
	target.RawQuery = query

	r := &runner{
		client:      client,
		target:      target.String(),
		mode:        o.mode,
		requests:    o.requests,
		concurrency: o.concurrency,
	}
	if o.redisURL != "" {
		redisOpts, err := goredis.ParseURL(o.redisURL)
		if err != nil {
			return nil, fmt.Errorf("redis URL: %w", err)
		}
		r.redis = goredis.NewClient(redisOpts)
		r.genKey = o.redisPrefix + ":tasks:list:generation"
		r.listMatch = o.redisPrefix + ":tasks:list:*"
	}
	return r, nil
}

func (r *runner) close() {
	if r.redis != nil {
		_ = r.redis.Close()
	}
}

// runPhase sends n requests across the fixed worker pool. The returned result
// is empty when n is zero.
func (r *runner) runPhase(ctx context.Context, n int) result {
	if n == 0 {
		return result{latencies: []time.Duration{}}
	}

	latencies := make([]time.Duration, n)
	succeeded := make([]bool, n)
	var failures atomic.Int64
	var mu sync.Mutex
	var samples []string
	recordFailure := func(err error) {
		failures.Add(1)
		mu.Lock()
		defer mu.Unlock()
		if len(samples) < 3 {
			samples = append(samples, err.Error())
		}
	}

	start := time.Now()
	var wg sync.WaitGroup
	for worker := 0; worker < r.concurrency; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < n; i += r.concurrency {
				elapsed, err := r.do(ctx)
				if err != nil {
					recordFailure(err)
					continue
				}
				latencies[i] = elapsed
				succeeded[i] = true
			}
		}(worker)
	}
	wg.Wait()
	wall := time.Since(start)

	// Compact to successful samples so the summary never counts a failed
	// request's timing.
	successful := latencies[:0]
	for i, d := range latencies {
		if succeeded[i] {
			successful = append(successful, d)
		}
	}
	return result{latencies: successful, failures: failures.Load(), wall: wall, samples: samples}
}

// do performs one request. In miss mode it first replaces the cache generation
// token, which makes the service's next read address a generation that has
// never been cached. That write is outside the timed region on purpose: it is
// test setup, not part of the measured request.
func (r *runner) do(ctx context.Context) (time.Duration, error) {
	if r.mode == modeMiss {
		if err := r.replaceGeneration(ctx); err != nil {
			return 0, fmt.Errorf("cache invalidation: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.target, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	// Bodies are discarded without inspection: the tool must never print task
	// contents.
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()

	_, readErr := io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)
	if readErr != nil {
		return 0, fmt.Errorf("read body: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return elapsed, nil
}

// replaceGeneration writes a fresh random token to the generation key. The
// service reads that key on every list request, so the request that follows
// cannot find a cached variant for the new generation. Tokens are 128 random
// bits, so a collision with a previously used generation is negligible.
func (r *runner) replaceGeneration(ctx context.Context) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if err := r.redis.Set(ctx, r.genKey, hex.EncodeToString(raw[:]), 0).Err(); err != nil {
		return err
	}
	return nil
}

// summary is the printed measurement.
type summary struct {
	Requests   int
	Successful int
	Failed     int
	Wall       time.Duration
	RPS        float64
	Mean       time.Duration
	P50        time.Duration
	P95        time.Duration
	P99        time.Duration
	Max        time.Duration
}

func summarize(latencies []time.Duration, wall time.Duration, failed int) summary {
	// Copy before sorting: the caller's sample order is irrelevant, but it is
	// not this function's data to reorder.
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)

	s := summary{
		Requests:   len(sorted) + failed,
		Successful: len(sorted),
		Failed:     failed,
		Wall:       wall,
	}
	if wall > 0 {
		s.RPS = float64(s.Requests) / wall.Seconds()
	}
	if len(sorted) == 0 {
		return s
	}

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	s.Mean = total / time.Duration(len(sorted))
	s.P50 = percentile(sorted, 50)
	s.P95 = percentile(sorted, 95)
	s.P99 = percentile(sorted, 99)
	s.Max = sorted[len(sorted)-1]
	return s
}

// percentile uses the nearest-rank definition on a sorted sample: the smallest
// observed value whose rank covers p% of the requests. It is never derived from
// the mean.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// cacheSnapshot is the Redis evidence used to prove which path the service
// took. It is collected only when -redis-url is set.
type cacheSnapshot struct {
	hits        int64
	misses      int64
	variantKeys int
}

func snapshotCache(ctx context.Context, client *goredis.Client, prefix, match string) (cacheSnapshot, error) {
	var snap cacheSnapshot

	info, err := client.Info(ctx, "stats").Result()
	if err != nil {
		return snap, fmt.Errorf("read redis stats: %w", err)
	}
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch key {
		case "keyspace_hits":
			snap.hits, _ = strconv.ParseInt(value, 10, 64)
		case "keyspace_misses":
			snap.misses, _ = strconv.ParseInt(value, 10, 64)
		}
	}

	// One list key exists per generation variant; the generation key itself has
	// one fewer colon-separated segment and is excluded here.
	prefixWithSep := prefix + ":tasks:list:"
	var cursor uint64
	for {
		keys, next, scanErr := client.Scan(ctx, cursor, match, 1000).Result()
		if scanErr != nil {
			return snap, fmt.Errorf("scan cache keys: %w", scanErr)
		}
		for _, key := range keys {
			if strings.Contains(strings.TrimPrefix(key, prefixWithSep), ":") {
				snap.variantKeys++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return snap, nil
}
