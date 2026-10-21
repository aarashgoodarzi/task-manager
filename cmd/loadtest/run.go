package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// run executes one scenario end to end: optional cache setup, optional dataset
// seeding, a warmup phase, the measured phase, cache evidence, and cleanup.
//
// Flag validation (including rejecting a base URL with a path) happens in
// options.validate before run is called.
func run(ctx context.Context, o options, out io.Writer) error {
	base, err := url.Parse(o.baseURL)
	if err != nil {
		// validate already checked this; keep a defensive guard for direct callers.
		return fmt.Errorf("%w: base URL: %v", errUsage, err)
	}

	transport := &http.Transport{
		MaxIdleConns:        o.concurrency * 2,
		MaxIdleConnsPerHost: o.concurrency,
		IdleConnTimeout:     30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	// The client timeout bounds dialing, headers, and body reads; it is the
	// explicit request timeout required by the scenario.
	client := &http.Client{Transport: transport, Timeout: o.timeout}

	r, err := newRunner(o, client, base)
	if err != nil {
		return err
	}
	defer r.close()

	fmt.Fprintf(out, "scenario\n")
	fmt.Fprintf(out, "  target       %s\n", r.target)
	fmt.Fprintf(out, "  mode         %s\n", modeDescription(o.mode))
	fmt.Fprintf(out, "  requests     %d\n", o.requests)
	fmt.Fprintf(out, "  concurrency  %d\n", o.concurrency)
	fmt.Fprintf(out, "  timeout      %s\n", o.timeout)
	if o.mode == modeWarm {
		fmt.Fprintf(out, "  warmup       %d\n", o.warmup)
		if o.warmup == 0 {
			fmt.Fprintf(out, "  note         no warmup; the first measured request is a cache miss\n")
		}
	}
	fmt.Fprintln(out)

	// Redis is optional only when -redis-url is omitted (warm mode without
	// evidence). When the URL is set, evidence is required and must match the
	// mode; miss mode already requires the URL for generation replacement.
	requireEvidence := o.redisURL != ""
	redisReady := false
	var before cacheSnapshot
	if r.redis != nil {
		if err := r.redis.Ping(ctx).Err(); err != nil {
			if o.mode == modeMiss || requireEvidence {
				return fmt.Errorf("%w: redis unreachable: %v", errEvidenceFailed, err)
			}
		} else {
			redisReady = true
		}
	} else if requireEvidence {
		return fmt.Errorf("%w: -redis-url was set but the Redis client was not configured", errEvidenceFailed)
	} else {
		fmt.Fprintf(out, "cache evidence skipped: pass -redis-url to require and verify Redis hits and misses\n\n")
	}

	var ds dataset
	if o.seed > 0 {
		marker := fmt.Sprintf("loadtest-%d", time.Now().UTC().UnixNano())
		ds, err = seedTasks(ctx, client, base, o.seed, marker)
		if err != nil {
			// Remove whatever this run managed to create before failing.
			if len(ds.IDs) > 0 {
				_, _, _ = cleanupTasks(ctx, client, base, ds.IDs)
			}
			return fmt.Errorf("seed dataset: %w", err)
		}
		fmt.Fprintf(out, "dataset\n")
		fmt.Fprintf(out, "  seeded       %d tasks in %s (title prefix %q, assignee \"loadtest\")\n", len(ds.IDs), ds.Took.Round(time.Millisecond), ds.Marker)
		fmt.Fprintf(out, "  total        %d -> %d (baseline %d + seeded %d)\n\n", ds.Baseline, ds.Total, ds.Baseline, len(ds.IDs))
	}

	cleanup := func() error {
		if len(ds.IDs) == 0 {
			return nil
		}
		deleted, _, cleanupErr := cleanupTasks(ctx, client, base, ds.IDs)
		fmt.Fprintf(out, "cleanup\n")
		fmt.Fprintf(out, "  deleted      %d of %d seeded tasks\n\n", deleted, len(ds.IDs))
		return cleanupErr
	}

	if o.mode == modeWarm && o.warmup > 0 {
		warm := r.runPhase(ctx, o.warmup)
		if warm.failures > 0 {
			fmt.Fprintf(out, "warmup: %d of %d requests failed\n", warm.failures, o.warmup)
			if !o.keep {
				_ = cleanup()
			}
			return fmt.Errorf("%w: warmup: %d of %d requests failed", errFailedRequests, warm.failures, o.warmup)
		}
	}

	if redisReady {
		before, err = snapshotCache(ctx, r.redis, o.redisPrefix, r.listMatch)
		if err != nil {
			if requireEvidence {
				if !o.keep {
					_ = cleanup()
				}
				return fmt.Errorf("%w: snapshot before measurement: %v", errEvidenceFailed, err)
			}
			redisReady = false
		}
	}

	measured := r.runPhase(ctx, o.requests)

	var after cacheSnapshot
	cacheOK := redisReady
	if cacheOK {
		after, err = snapshotCache(ctx, r.redis, o.redisPrefix, r.listMatch)
		if err != nil {
			if requireEvidence {
				if !o.keep {
					_ = cleanup()
				}
				return fmt.Errorf("%w: snapshot after measurement: %v", errEvidenceFailed, err)
			}
			cacheOK = false
		}
	}

	// Cleanup happens after the measurement so the deletes cannot invalidate the
	// cache mid-run. The seeded rows are exactly the IDs recorded above.
	var cleanupErr error
	if !o.keep {
		cleanupErr = cleanup()
	} else if len(ds.IDs) > 0 {
		fmt.Fprintf(out, "cleanup skipped (-keep): %d tasks titled %q remain\n\n", len(ds.IDs), ds.Marker)
	}

	s := summarize(measured.latencies, measured.wall, int(measured.failures))
	fmt.Fprintf(out, "results\n")
	fmt.Fprintf(out, "  total        %d\n", s.Requests)
	fmt.Fprintf(out, "  successful   %d\n", s.Successful)
	fmt.Fprintf(out, "  failed       %d\n", s.Failed)
	fmt.Fprintf(out, "  wall clock   %s\n", s.Wall.Round(time.Microsecond))
	fmt.Fprintf(out, "  throughput   %.1f req/s\n", s.RPS)
	fmt.Fprintf(out, "  mean         %s\n", round(s.Mean))
	fmt.Fprintf(out, "  p50          %s\n", round(s.P50))
	fmt.Fprintf(out, "  p95          %s\n", round(s.P95))
	fmt.Fprintf(out, "  p99          %s\n", round(s.P99))
	fmt.Fprintf(out, "  max          %s\n", round(s.Max))
	fmt.Fprintln(out)

	if len(measured.samples) > 0 {
		fmt.Fprintf(out, "failures (first %d)\n", len(measured.samples))
		for _, sample := range measured.samples {
			fmt.Fprintf(out, "  %s\n", sample)
		}
		fmt.Fprintln(out)
	}

	var evidenceErr error
	if cacheOK {
		delta := evidenceDelta{
			hits:         after.hits - before.hits,
			misses:       after.misses - before.misses,
			variantDelta: after.variantKeys - before.variantKeys,
			afterKeys:    after.variantKeys,
		}
		fmt.Fprintf(out, "cache evidence (%s, prefix %q)\n", o.redisURL, o.redisPrefix)
		fmt.Fprintf(out, "  keyspace     hits +%d, misses +%d during the measured phase\n", delta.hits, delta.misses)
		fmt.Fprintf(out, "  list keys    %d -> %d (delta %+d)\n", before.variantKeys, after.variantKeys, delta.variantDelta)
		evidenceErr = evaluateCacheEvidence(o.mode, o.requests, delta)
		if evidenceErr != nil {
			fmt.Fprintf(out, "  verdict      FAILED: %v\n", evidenceErr)
		} else {
			fmt.Fprintf(out, "  verdict      OK for %s mode\n", o.mode)
		}
		fmt.Fprintln(out)
	} else if requireEvidence {
		evidenceErr = fmt.Errorf("%w: required Redis evidence was not collected", errEvidenceFailed)
	}

	if cleanupErr != nil {
		return fmt.Errorf("cleanup after measurement: %w", cleanupErr)
	}
	if s.Failed > 0 {
		return fmt.Errorf("%w: %d of %d requests failed", errFailedRequests, s.Failed, s.Requests)
	}
	if evidenceErr != nil {
		return evidenceErr
	}
	return nil
}

func modeDescription(mode string) string {
	if mode == modeMiss {
		return "miss (generation token replaced before each request, PostgreSQL-backed reads)"
	}
	return "warm (same query, Redis list cache)"
}

func round(d time.Duration) time.Duration {
	if d == 0 {
		return 0
	}
	return d.Round(10 * time.Microsecond)
}
