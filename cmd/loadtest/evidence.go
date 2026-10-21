package main

import "fmt"

// evidenceDelta is the Redis counter and key change observed across the
// measured phase. It is only evaluated when -redis-url was supplied and both
// snapshots succeeded.
type evidenceDelta struct {
	hits         int64
	misses       int64
	variantDelta int
	afterKeys    int
}

// evaluateCacheEvidence enforces the mode claims from ADR 0015 once evidence
// has been collected. Optional absence of -redis-url is handled by the caller;
// this function only judges contradictory or incomplete proof.
func evaluateCacheEvidence(mode string, requests int, d evidenceDelta) error {
	switch mode {
	case modeWarm:
		if d.misses != 0 {
			return fmt.Errorf("%w: warm mode observed %d keyspace misses during the measured phase; want 0", errEvidenceFailed, d.misses)
		}
		if d.variantDelta != 0 {
			return fmt.Errorf("%w: warm mode changed list-key count by %+d; want 0 (no new keys)", errEvidenceFailed, d.variantDelta)
		}
		if d.afterKeys == 0 {
			return fmt.Errorf("%w: warm mode found no cached list keys; the cache path was not exercised", errEvidenceFailed)
		}
	case modeMiss:
		if int(d.variantDelta) < requests {
			return fmt.Errorf("%w: miss mode wrote %+d list keys for %d requests; want at least one new key per request", errEvidenceFailed, d.variantDelta, requests)
		}
		if d.misses < int64(requests) {
			return fmt.Errorf("%w: miss mode observed %d keyspace misses for %d requests; want at least one miss per request", errEvidenceFailed, d.misses, requests)
		}
	default:
		return fmt.Errorf("%w: unknown mode %q", errEvidenceFailed, mode)
	}
	return nil
}
