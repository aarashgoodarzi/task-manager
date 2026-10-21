package redis

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// fakeStore replaces Redis with an in-memory map. It is small on purpose: the
// adapter's generation and failure rules should be testable without a server,
// while real command semantics are covered by the integration suite.
type fakeStore struct {
	values map[string]string
	ttls   map[string]time.Duration

	getErr   error
	setErr   error
	setNXErr error
	closeErr error

	setNXResult bool
	closed      bool

	getCalls   int
	setCalls   int
	setNXCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		values:      make(map[string]string),
		ttls:        make(map[string]time.Duration),
		setNXResult: true,
	}
}

func (f *fakeStore) Get(_ context.Context, key string) (string, error) {
	f.getCalls++
	if f.getErr != nil {
		return "", f.getErr
	}
	value, ok := f.values[key]
	if !ok {
		return "", goredis.Nil
	}
	return value, nil
}

func (f *fakeStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.values[key] = value
	f.ttls[key] = ttl
	return nil
}

func (f *fakeStore) SetNX(_ context.Context, key, value string) (bool, error) {
	f.setNXCalls++
	if f.setNXErr != nil {
		return false, f.setNXErr
	}
	if !f.setNXResult {
		return false, nil
	}
	if _, exists := f.values[key]; exists {
		return false, nil
	}
	f.values[key] = value
	return true, nil
}

func (f *fakeStore) Close() error {
	f.closed = true
	return f.closeErr
}

// tokenSequence makes generation tokens deterministic.
func tokenSequence(tokens ...string) func() (string, error) {
	index := 0
	return func() (string, error) {
		token := tokens[index]
		index++
		return token, nil
	}
}

func newTestCache(store store, ttl time.Duration, tokens ...string) *Cache {
	return &Cache{
		store:    store,
		ttl:      ttl,
		prefix:   "test",
		newToken: tokenSequence(tokens...),
		now:      time.Now,
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{TTL: time.Minute}); err == nil {
		t.Fatal("New() without URL: want error, got nil")
	}
	if _, err := New(Config{URL: "redis://localhost:6379/0"}); err == nil {
		t.Fatal("New() without TTL: want error, got nil")
	}
	if _, err := New(Config{URL: "://not-a-url", TTL: time.Minute}); err == nil {
		t.Fatal("New() with a malformed URL: want error, got nil")
	}

	cache, err := New(Config{URL: "redis://127.0.0.1:1/0", TTL: time.Minute})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}
}

func TestGetMissInitializesGeneration(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")

	snapshot, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if snapshot.Hit {
		t.Fatal("Get() reported a hit for an empty cache")
	}
	if snapshot.Token != "gen-a" {
		t.Fatalf("token = %q, want the initialized generation", snapshot.Token)
	}
	if got := store.values[cache.generationKey()]; got != "gen-a" {
		t.Fatalf("generation key = %q, want gen-a", got)
	}
	if store.setNXCalls != 1 {
		t.Fatalf("SetNX calls = %d, want 1", store.setNXCalls)
	}
}

func TestGetHitRoundTrip(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")

	miss, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}

	tasks := []domain.Task{
		{ID: "id-1", Title: "first", Description: "one", Status: domain.StatusTodo},
		{ID: "id-2", Title: "second", Description: "", Status: domain.StatusDone},
	}
	if err := cache.Set(context.Background(), service.ListSnapshot{Tasks: tasks, Token: miss.Token}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	hit, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if !hit.Hit {
		t.Fatal("Get() missed after Set()")
	}
	if len(hit.Tasks) != 2 || hit.Tasks[0] != tasks[0] || hit.Tasks[1] != tasks[1] {
		t.Fatalf("tasks = %+v, want %+v", hit.Tasks, tasks)
	}
	if hit.Token != miss.Token {
		t.Fatalf("token = %q, want %q", hit.Token, miss.Token)
	}
}

func TestGetHitEmptyList(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")

	miss, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if err := cache.Set(context.Background(), service.ListSnapshot{
		Tasks: []domain.Task{},
		Token: miss.Token,
	}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	hit, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if !hit.Hit {
		t.Fatal("empty list was not treated as a hit")
	}
	if hit.Tasks == nil || len(hit.Tasks) != 0 {
		t.Fatalf("tasks = %#v, want a non-nil empty list", hit.Tasks)
	}
}

func TestGetMalformedPayloadFallsBackWithError(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")

	miss, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	store.values[cache.listKey(miss.Token)] = "{not json"

	hit, err := cache.Get(context.Background())
	if err == nil {
		t.Fatal("Get() with malformed JSON: want error, got nil")
	}
	if hit.Hit {
		t.Fatal("Get() reported a hit for malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode list") {
		t.Fatalf("error = %v, want a decode error", err)
	}
}

func TestGetUnsupportedPayloadVersionFallsBackWithError(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")

	miss, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	payload, _ := json.Marshal(cachedList{Version: cacheSchemaVersion + 1})
	store.values[cache.listKey(miss.Token)] = string(payload)

	if _, err := cache.Get(context.Background()); err == nil {
		t.Fatal("Get() with an unsupported version: want error, got nil")
	}
}

func TestSetRejectsMissingToken(t *testing.T) {
	cache := newTestCache(newFakeStore(), time.Minute, "gen-a")

	if err := cache.Set(context.Background(), service.ListSnapshot{}); err == nil {
		t.Fatal("Set() without a token: want error, got nil")
	}
}

func TestSetUsesConfiguredTTLAndKeyLayout(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, 90*time.Second, "gen-a")

	if err := cache.Set(context.Background(), service.ListSnapshot{
		Tasks: []domain.Task{{ID: "id-1", Title: "task", Status: domain.StatusTodo}},
		Token: "gen-a",
	}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	key := "test:tasks:list:gen-a"
	if _, ok := store.values[key]; !ok {
		t.Fatalf("value keys = %v, want %q", store.values, key)
	}
	if got := store.ttls[key]; got != 90*time.Second {
		t.Fatalf("TTL = %v, want 90s", got)
	}

	var payload cachedList
	if err := json.Unmarshal([]byte(store.values[key]), &payload); err != nil {
		t.Fatalf("decode stored payload: %v", err)
	}
	if payload.Version != cacheSchemaVersion || len(payload.Tasks) != 1 {
		t.Fatalf("payload = %+v, want version %d with one task", payload, cacheSchemaVersion)
	}
}

func TestInvalidateStartsNewGeneration(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")

	first, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if err := cache.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate() unexpected error: %v", err)
	}
	second, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}

	if second.Token == first.Token {
		t.Fatalf("token after invalidation = %q, want a new generation", second.Token)
	}
	if store.values[cache.generationKey()] != "gen-b" {
		t.Fatalf("generation key = %q, want gen-b", store.values[cache.generationKey()])
	}
}

func TestInvalidationFailureDegradesReadsAndRecovers(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")
	store.setErr = errors.New("redis unavailable")

	if err := cache.Invalidate(context.Background()); err == nil {
		t.Fatal("Invalidate() failure: want error, got nil")
	}
	if _, err := cache.Get(context.Background()); !errors.Is(err, errDegraded) {
		t.Fatalf("Get() error = %v, want errDegraded while invalidation is outstanding", err)
	}

	store.setErr = nil
	if err := cache.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate() recovery: unexpected error: %v", err)
	}
	if _, err := cache.Get(context.Background()); err != nil {
		t.Fatalf("Get() after recovery: unexpected error: %v", err)
	}
}

// TestDegradedCacheRecoversAfterCooldown proves a failed invalidation disables
// caching only until the next successful probe, and that the probe starts a new
// generation instead of trusting a list that may have missed a mutation.
func TestDegradedCacheRecoversAfterCooldown(t *testing.T) {
	store := newFakeStore()
	// gen-a is consumed by the failing invalidation, gen-b by the recovery.
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")
	now := time.Now()
	cache.now = func() time.Time { return now }

	store.setErr = errors.New("redis unavailable")
	if err := cache.Invalidate(context.Background()); err == nil {
		t.Fatal("Invalidate() failure: want error, got nil")
	}

	// Reads during the cooldown must not dial Redis on every request.
	before := store.getCalls
	if _, err := cache.Get(context.Background()); !errors.Is(err, errDegraded) {
		t.Fatalf("Get() error = %v, want errDegraded during the cooldown", err)
	}
	if store.getCalls != before {
		t.Fatalf("degraded Get() touched Redis (%d reads); want reads bypassed during the cooldown", store.getCalls-before)
	}

	// Redis comes back: the first read after the cooldown re-establishes the
	// cache instead of staying degraded until the next mutation.
	store.setErr = nil
	now = now.Add(recoveryInterval + time.Second)

	snapshot, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() after the cooldown: unexpected error: %v", err)
	}
	if snapshot.Hit {
		t.Fatal("Get() after recovery reported a hit for a fresh generation")
	}
	if snapshot.Token != "gen-b" {
		t.Fatalf("token = %q, want the recovery generation", snapshot.Token)
	}

	if err := cache.Set(context.Background(), service.ListSnapshot{
		Tasks: []domain.Task{{ID: "id-1", Title: "fresh", Status: domain.StatusTodo}},
		Token: snapshot.Token,
	}); err != nil {
		t.Fatalf("Set() after recovery: unexpected error: %v", err)
	}
	hit, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() after refill: unexpected error: %v", err)
	}
	if !hit.Hit {
		t.Fatal("cache did not resume after recovery")
	}
}

func TestDegradedCacheRetriesAtMostOncePerInterval(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")
	now := time.Now()
	cache.now = func() time.Time { return now }

	store.setErr = errors.New("redis unavailable")
	if err := cache.Invalidate(context.Background()); err == nil {
		t.Fatal("Invalidate() failure: want error, got nil")
	}

	setAttempts := store.setCalls
	now = now.Add(recoveryInterval + time.Second)
	for i := 0; i < 3; i++ {
		if _, err := cache.Get(context.Background()); err == nil {
			t.Fatalf("Get() %d: want an error while Redis is still down", i)
		}
	}

	if got := store.setCalls - setAttempts; got != 1 {
		t.Fatalf("recovery probes = %d, want 1 within one interval", got)
	}
}

func TestStaleRefillAfterInvalidationIsNotReadable(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")

	// A GET misses and reads the old generation.
	stale, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if stale.Hit {
		t.Fatal("first Get() unexpectedly hit")
	}

	// A mutation commits and invalidates while that GET is still running.
	if err := cache.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate() unexpected error: %v", err)
	}

	// The slow GET publishes its old result under the old generation.
	if err := cache.Set(context.Background(), service.ListSnapshot{
		Tasks: []domain.Task{{ID: "stale", Title: "stale", Status: domain.StatusTodo}},
		Token: stale.Token,
	}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	// New readers use the new generation and must not see the stale write.
	got, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if got.Hit {
		t.Fatalf("Get() hit stale data after invalidation: %+v", got.Tasks)
	}
}

func TestCloseClosesStore(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")

	if err := cache.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}
	if !store.closed {
		t.Fatal("store was not closed")
	}

	var nilCache *Cache
	if err := nilCache.Close(); err != nil {
		t.Fatalf("nil Close() error = %v, want nil", err)
	}
}
