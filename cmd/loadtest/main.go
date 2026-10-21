// Command loadtest runs a bounded, reproducible HTTP load test against the
// task-list endpoint (GET /tasks).
//
// It is a measurement tool, not part of the service. It sends a fixed number of
// requests with a fixed concurrency, reports wall-clock throughput and latency
// percentiles, and can seed and remove its own dataset so repeated runs measure
// the same table size.
//
// Two cache modes are supported when Redis is configured:
//
//	warm  every request uses the same query, so after a short warmup the
//	      service answers from the Redis list cache.
//	      miss  each request first replaces the cache generation token with a
//	      fresh one, so the following GET cannot be served from Redis and
//	      must read PostgreSQL. This mode is serialized (concurrency 1) so a
//	      request repopulating the cache cannot turn later requests into hits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// errFailedRequests reports that the scenario ran but at least one request
// failed. The command exits unsuccessfully so a failing target cannot look like
// a successful measurement in CI or a script.
var errFailedRequests = errors.New("one or more requests failed")

// errEvidenceFailed reports that Redis cache evidence contradicted the
// requested mode (or could not be collected after -redis-url was supplied).
// Exit status is 1: the run is not a valid benchmark claim.
var errEvidenceFailed = errors.New("cache evidence failed")

// errUsage marks invalid flags or options; main maps it to exit status 2.
var errUsage = errors.New("invalid usage")

const (
	modeWarm = "warm"
	modeMiss = "miss"

	// maxRequests bounds the latency sample slice (8 bytes per sample) so a
	// typo cannot exhaust memory.
	maxRequests = 1_000_000
	// maxConcurrency keeps goroutine and connection counts deliberate.
	maxConcurrency = 4096
	// maxWarmup bounds the priming phase for the same reason as maxRequests.
	maxWarmup = 100_000
)

type options struct {
	baseURL     string
	query       string
	requests    int
	concurrency int
	timeout     time.Duration
	warmup      int
	mode        string
	redisURL    string
	redisPrefix string
	seed        int
	keep        bool
	allowRemote bool
}

func parseFlags(args []string, out io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.baseURL, "base-url", "http://127.0.0.1:8080", "service base URL")
	fs.StringVar(&o.query, "query", "page=1&page_size=20", "query string for GET /tasks (leading ? is optional)")
	fs.IntVar(&o.requests, "requests", 1000, "measured requests (1..1000000)")
	fs.IntVar(&o.concurrency, "concurrency", 8, "concurrent workers (1..4096)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout")
	fs.IntVar(&o.warmup, "warmup", 20, "requests sent before measuring to prime the cache (warm mode)")
	fs.StringVar(&o.mode, "mode", modeWarm, "cache mode: warm or miss")
	fs.StringVar(&o.redisURL, "redis-url", "", "Redis URL (required for miss mode; when set, cache evidence is required and must match the mode)")
	fs.StringVar(&o.redisPrefix, "redis-prefix", "task-manager", "cache key prefix used by the service")
	fs.IntVar(&o.seed, "seed", 0, "create this many tasks before measuring and delete them afterwards (0 disables)")
	fs.BoolVar(&o.keep, "keep", false, "keep seeded tasks instead of deleting them")
	fs.BoolVar(&o.allowRemote, "allow-remote-mutations", false, "allow -seed against a non-loopback base URL")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: go run ./cmd/loadtest [flags]\n\n")
		fmt.Fprintf(out, "Load-tests GET /tasks and prints measured latency and throughput.\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("%w: unexpected arguments: %s", errUsage, strings.Join(fs.Args(), " "))
	}
	if err := o.validate(); err != nil {
		return options{}, err
	}
	return o, nil
}

func (o options) validate() error {
	base, err := url.Parse(o.baseURL)
	if err != nil {
		return fmt.Errorf("%w: base URL: %v", errUsage, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return fmt.Errorf("%w: base URL must use http or https, got %q", errUsage, base.Scheme)
	}
	if base.Host == "" {
		return fmt.Errorf("%w: base URL %q has no host", errUsage, o.baseURL)
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("%w: pass list parameters with -query, not in -base-url", errUsage)
	}
	if base.Path != "" && base.Path != "/" {
		return fmt.Errorf("%w: base URL must not include a path; pass list parameters with -query", errUsage)
	}
	if _, err := url.ParseQuery(strings.TrimPrefix(o.query, "?")); err != nil {
		return fmt.Errorf("%w: query: %v", errUsage, err)
	}
	if o.requests < 1 || o.requests > maxRequests {
		return fmt.Errorf("%w: -requests must be between 1 and %d", errUsage, maxRequests)
	}
	if o.concurrency < 1 || o.concurrency > maxConcurrency {
		return fmt.Errorf("%w: -concurrency must be between 1 and %d", errUsage, maxConcurrency)
	}
	if o.timeout <= 0 {
		return fmt.Errorf("%w: -timeout must be positive", errUsage)
	}
	if o.warmup < 0 || o.warmup > maxWarmup {
		return fmt.Errorf("%w: -warmup must be between 0 and %d", errUsage, maxWarmup)
	}
	if o.mode != modeWarm && o.mode != modeMiss {
		return fmt.Errorf("%w: -mode must be %q or %q", errUsage, modeWarm, modeMiss)
	}
	if o.seed < 0 {
		return fmt.Errorf("%w: -seed must not be negative", errUsage)
	}
	if o.mode == modeMiss {
		if o.redisURL == "" {
			// Without Redis there is no generation token to replace, so a
			// "miss" run could not be verified and must not be labelled one.
			return fmt.Errorf("%w: -mode miss requires -redis-url", errUsage)
		}
		if o.concurrency != 1 {
			return fmt.Errorf("%w: -mode miss requires -concurrency 1 so each request is a verified miss", errUsage)
		}
	}
	if o.redisPrefix == "" {
		return fmt.Errorf("%w: -redis-prefix must not be empty", errUsage)
	}
	if (o.seed > 0 || o.keep) && !isLoopbackURL(base) && !o.allowRemote {
		return fmt.Errorf("%w: refusing to create or delete tasks on non-loopback host %q without -allow-remote-mutations", errUsage, base.Host)
	}
	return nil
}

func isLoopbackURL(u *url.URL) bool {
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// exitCode maps failures to shell statuses: 2 for unusable options, 1 for a
// measurement that ran but did not succeed (failed requests, failed warmup,
// contradictory or missing required cache evidence, transport errors, or a
// cleanup that could not finish), 0 only for a fully successful run.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		return 1
	}
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(exitCode(err))
	}

	if err := run(context.Background(), o, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(exitCode(err))
	}
}
