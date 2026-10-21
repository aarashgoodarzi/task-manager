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

	getErr              error
	setErr              error
	setNXErr            error
	compareAndDeleteErr error
	closeErr            error

	setNXResult bool
	closed      bool

	getCalls              int
	setCalls              int
	setNXCalls            int
	compareAndDeleteCalls int

	// beforeCompareAndDelete runs after Get has observed a value and before
	// CompareAndDelete decides whether to remove it. Tests use it to install a
	// concurrent replacement without sleeps.
	beforeCompareAndDelete func(key, expected string)
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

func (f *fakeStore) CompareAndDelete(_ context.Context, key, expected string) (bool, error) {
	f.compareAndDeleteCalls++
	if f.beforeCompareAndDelete != nil {
		f.beforeCompareAndDelete(key, expected)
	}
	if f.compareAndDeleteErr != nil {
		return false, f.compareAndDeleteErr
	}
	value, ok := f.values[key]
	if !ok || value != expected {
		return false, nil
	}
	delete(f.values, key)
	delete(f.ttls, key)
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

// defaultQuery is the query the service produces for a request with no
// parameters, which most tests exercise.
func defaultQuery() service.ListQuery {
	return service.ListQuery{PageSize: service.DefaultPageSize}
}

// samePage compares two cached pages field by field; ListResult holds a slice
// and is therefore not comparable.
func samePage(a, b service.ListResult) bool {
	if a.PageSize != b.PageSize || a.NextCursor != b.NextCursor || a.HasMore != b.HasMore {
		return false
	}
	if len(a.Tasks) != len(b.Tasks) {
		return false
	}
	for i := range a.Tasks {
		if a.Tasks[i] != b.Tasks[i] {
			return false
		}
	}
	return true
}

func pageOf(tasks ...domain.Task) service.ListResult {
	return service.ListResult{
		Tasks:    tasks,
		PageSize: service.DefaultPageSize,
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

	snapshot, err := cache.Get(context.Background(), defaultQuery())
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
	query := defaultQuery()

	miss, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}

	tasks := []domain.Task{
		{ID: "id-1", Title: "first", Description: "one", Status: domain.StatusTodo, Assignee: "alice"},
		{ID: "id-2", Title: "second", Description: "", Status: domain.StatusDone},
	}
	result := service.ListResult{Tasks: tasks, PageSize: 20}
	if err := cache.Set(context.Background(), query, service.ListSnapshot{Result: result, Token: miss.Token}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	hit, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if !hit.Hit {
		t.Fatal("Get() missed after Set()")
	}
	if len(hit.Result.Tasks) != 2 || hit.Result.Tasks[0] != tasks[0] || hit.Result.Tasks[1] != tasks[1] {
		t.Fatalf("tasks = %+v, want %+v", hit.Result.Tasks, tasks)
	}
	if !samePage(hit.Result, result) {
		t.Fatalf("result = %+v, want the cached page metadata %+v", hit.Result, result)
	}
	if hit.Token != miss.Token {
		t.Fatalf("token = %q, want %q", hit.Token, miss.Token)
	}
}

func TestGetHitEmptyPage(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")
	query := defaultQuery()

	miss, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	result := service.ListResult{Tasks: []domain.Task{}, PageSize: 10, NextCursor: "next", HasMore: true}
	if err := cache.Set(context.Background(), query, service.ListSnapshot{Result: result, Token: miss.Token}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	hit, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if !hit.Hit {
		t.Fatal("empty page was not treated as a hit")
	}
	if hit.Result.Tasks == nil || len(hit.Result.Tasks) != 0 {
		t.Fatalf("tasks = %#v, want a non-nil empty page", hit.Result.Tasks)
	}
	if hit.Result.PageSize != 10 || !hit.Result.HasMore || hit.Result.NextCursor != "next" {
		t.Fatalf("result = %+v, want the cached cursor metadata", hit.Result)
	}
}

func TestGetMalformedPayloadIsTreatedAsMissAndRemoved(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")
	query := defaultQuery()

	miss, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	store.values[key] = "{not json"

	got, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() with malformed JSON: unexpected error %v", err)
	}
	if got.Hit {
		t.Fatal("Get() reported a hit for malformed JSON")
	}
	if got.Token != miss.Token {
		t.Fatalf("token = %q, want the observed generation %q so Set can refill", got.Token, miss.Token)
	}
	if store.compareAndDeleteCalls != 1 {
		t.Fatalf("CompareAndDelete called %d times, want 1", store.compareAndDeleteCalls)
	}
	if _, ok := store.values[key]; ok {
		t.Fatal("malformed payload was left in the store")
	}

	// A miss with a token lets the service refill; the next Get must then hit.
	page := pageOf(domain.Task{ID: "id-1", Title: "task", Status: domain.StatusTodo})
	if err := cache.Set(context.Background(), query, service.ListSnapshot{Result: page, Token: got.Token}); err != nil {
		t.Fatalf("Set() after poison recovery: %v", err)
	}
	hit, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() after refill: %v", err)
	}
	if !hit.Hit || !samePage(hit.Result, page) {
		t.Fatalf("Get() after refill = hit=%v result=%+v, want the refilled page", hit.Hit, hit.Result)
	}
}

func TestGetUnsupportedPayloadVersionIsTreatedAsMissAndRemoved(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")
	query := defaultQuery()

	miss, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	payload, _ := json.Marshal(cachedList{Version: cacheSchemaVersion + 1})
	store.values[key] = string(payload)

	got, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() with unsupported version: unexpected error %v", err)
	}
	if got.Hit {
		t.Fatal("Get() reported a hit for an unsupported version")
	}
	if got.Token != miss.Token {
		t.Fatalf("token = %q, want %q", got.Token, miss.Token)
	}
	if _, ok := store.values[key]; ok {
		t.Fatal("unsupported-version payload was left in the store")
	}
}

func TestGetInvalidPayloadDoesNotDeleteConcurrentReplacement(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")
	query := defaultQuery()

	miss, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	poison := "{not json"
	store.values[key] = poison

	valid, err := json.Marshal(cachedList{
		Version:  cacheSchemaVersion,
		Tasks:    []domain.Task{{ID: "id-1", Title: "fresh", Status: domain.StatusTodo}},
		PageSize: service.DefaultPageSize,
	})
	if err != nil {
		t.Fatalf("marshal valid payload: %v", err)
	}

	// After this reader observed the poison value, a concurrent refill writes
	// a valid payload under the same key. Compare-and-delete must leave it.
	store.beforeCompareAndDelete = func(gotKey, expected string) {
		if gotKey != key {
			t.Fatalf("CompareAndDelete key = %q, want %q", gotKey, key)
		}
		if expected != poison {
			t.Fatalf("CompareAndDelete expected = %q, want the observed poison", expected)
		}
		store.values[key] = string(valid)
		store.ttls[key] = time.Minute
	}

	got, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if got.Hit {
		t.Fatal("Get() that observed poison must not report a hit")
	}
	if store.values[key] != string(valid) {
		t.Fatalf("store value = %q, want the concurrent valid payload preserved", store.values[key])
	}
}

func TestGetInvalidPayloadStillMissesWhenCompareAndDeleteFails(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")
	query := defaultQuery()

	miss, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	store.values[key] = "{not json"
	store.compareAndDeleteErr = errors.New("redis unavailable")

	got, err := cache.Get(context.Background(), query)
	if err != nil {
		t.Fatalf("Get() must remain fail-open when discard fails: %v", err)
	}
	if got.Hit || got.Token != miss.Token {
		t.Fatalf("Get() = %+v, want a miss carrying the observed token", got)
	}
	if store.values[key] != "{not json" {
		t.Fatal("failed discard must not invent a deletion")
	}
}

func TestSetRejectsMissingToken(t *testing.T) {
	cache := newTestCache(newFakeStore(), time.Minute, "gen-a")

	if err := cache.Set(context.Background(), defaultQuery(), service.ListSnapshot{}); err == nil {
		t.Fatal("Set() without a token: want error, got nil")
	}
}

func TestSetUsesConfiguredTTLAndKeyLayout(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, 90*time.Second, "gen-a")
	query := defaultQuery()

	if err := cache.Set(context.Background(), query, service.ListSnapshot{
		Result: pageOf(domain.Task{ID: "id-1", Title: "task", Status: domain.StatusTodo}),
		Token:  "gen-a",
	}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	key := cache.listKey("gen-a", query)
	if _, ok := store.values[key]; !ok {
		t.Fatalf("value keys = %v, want a variant key under the generation", store.values)
	}
	if !strings.HasPrefix(key, "test:tasks:list:gen-a:") {
		t.Fatalf("key = %q, want the documented generation/variant layout", key)
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
	if payload.PageSize != service.DefaultPageSize || payload.HasMore || payload.NextCursor != "" {
		t.Fatalf("payload = %+v, want the pagination metadata", payload)
	}
}

// TestVariantKeysAreDistinct proves that every response-affecting parameter
// names its own entry, so one query can never answer another.
func TestVariantKeysAreDistinct(t *testing.T) {
	base := defaultQuery()

	queries := map[string]service.ListQuery{
		"base":           base,
		"status todo":    {Status: domain.StatusTodo, PageSize: 20},
		"status done":    {Status: domain.StatusDone, PageSize: 20},
		"assignee alice": {Assignee: "alice", PageSize: 20},
		"assignee bob":   {Assignee: "bob", PageSize: 20},
		"page 2":         {Cursor: "cursor-page-2", PageSize: 20},
		"page size 10":   {PageSize: 10},
		"both filters":   {Status: domain.StatusDone, Assignee: "alice", PageSize: 20},
	}

	seen := make(map[string]string, len(queries))
	for name, query := range queries {
		key := variantKey(query)
		if existing, duplicate := seen[key]; duplicate {
			t.Fatalf("%q and %q share cache key %q", existing, name, key)
		}
		seen[key] = name
	}
}

// TestVariantKeyIsStableForEquivalentQueries pins normalization: the same
// effective query must reuse one entry, so the defaults cannot double the cache.
func TestVariantKeyIsStableForEquivalentQueries(t *testing.T) {
	cache := newTestCache(newFakeStore(), time.Minute, "gen-a")

	defaultKey := cache.listKey("gen-a", defaultQuery())
	explicitKey := cache.listKey("gen-a", service.ListQuery{PageSize: service.DefaultPageSize})
	if defaultKey != explicitKey {
		t.Fatalf("keys differ for equivalent queries: %q vs %q", defaultKey, explicitKey)
	}
}

func TestVariantsAreIndependent(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a")
	ctx := context.Background()

	aliceQuery := service.ListQuery{Assignee: "alice", PageSize: 20}
	bobQuery := service.ListQuery{Assignee: "bob", PageSize: 20}

	miss, err := cache.Get(ctx, aliceQuery)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if err := cache.Set(ctx, aliceQuery, service.ListSnapshot{
		Result: service.ListResult{
			Tasks:    []domain.Task{{ID: "alice-task", Title: "alice", Assignee: "alice", Status: domain.StatusTodo}},
			PageSize: 20,
		},
		Token: miss.Token,
	}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	aliceHit, err := cache.Get(ctx, aliceQuery)
	if err != nil {
		t.Fatalf("Get(alice) unexpected error: %v", err)
	}
	if !aliceHit.Hit || aliceHit.Result.Tasks[0].ID != "alice-task" {
		t.Fatalf("alice page = %+v, want the cached alice task", aliceHit.Result)
	}

	bobMiss, err := cache.Get(ctx, bobQuery)
	if err != nil {
		t.Fatalf("Get(bob) unexpected error: %v", err)
	}
	if bobMiss.Hit {
		t.Fatalf("bob query hit alice's cached page: %+v", bobMiss.Result)
	}
	if bobMiss.Result.Tasks != nil {
		t.Fatalf("miss carried tasks: %+v", bobMiss.Result.Tasks)
	}
}

// TestInvalidationHidesEveryVariant proves one mutation invalidates the whole
// family of query entries, not just the one that happens to be at hand.
func TestInvalidationHidesEveryVariant(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")
	ctx := context.Background()

	queries := []service.ListQuery{
		{PageSize: 20},
		{Status: domain.StatusDone, PageSize: 20},
		{Assignee: "alice", Cursor: "cursor-alice-2", PageSize: 5},
	}

	for _, query := range queries {
		miss, err := cache.Get(ctx, query)
		if err != nil {
			t.Fatalf("Get() unexpected error: %v", err)
		}
		if err := cache.Set(ctx, query, service.ListSnapshot{Result: pageOf(), Token: miss.Token}); err != nil {
			t.Fatalf("Set() unexpected error: %v", err)
		}
	}

	if err := cache.Invalidate(ctx); err != nil {
		t.Fatalf("Invalidate() unexpected error: %v", err)
	}

	for _, query := range queries {
		hit, err := cache.Get(ctx, query)
		if err != nil {
			t.Fatalf("Get() after invalidation: unexpected error: %v", err)
		}
		if hit.Hit {
			t.Fatalf("query %+v still hit after invalidation", query)
		}
	}
}

func TestInvalidateStartsNewGeneration(t *testing.T) {
	store := newFakeStore()
	cache := newTestCache(store, time.Minute, "gen-a", "gen-b")
	ctx := context.Background()

	first, err := cache.Get(ctx, defaultQuery())
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if err := cache.Invalidate(ctx); err != nil {
		t.Fatalf("Invalidate() unexpected error: %v", err)
	}
	second, err := cache.Get(ctx, defaultQuery())
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
	if _, err := cache.Get(context.Background(), defaultQuery()); !errors.Is(err, errDegraded) {
		t.Fatalf("Get() error = %v, want errDegraded while invalidation is outstanding", err)
	}

	store.setErr = nil
	if err := cache.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate() recovery: unexpected error: %v", err)
	}
	if _, err := cache.Get(context.Background(), defaultQuery()); err != nil {
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
	if _, err := cache.Get(context.Background(), defaultQuery()); !errors.Is(err, errDegraded) {
		t.Fatalf("Get() error = %v, want errDegraded during the cooldown", err)
	}
	if store.getCalls != before {
		t.Fatalf("degraded Get() touched Redis (%d reads); want reads bypassed during the cooldown", store.getCalls-before)
	}

	// Redis comes back: the first read after the cooldown re-establishes the
	// cache instead of staying degraded until the next mutation.
	store.setErr = nil
	now = now.Add(recoveryInterval + time.Second)

	snapshot, err := cache.Get(context.Background(), defaultQuery())
	if err != nil {
		t.Fatalf("Get() after the cooldown: unexpected error: %v", err)
	}
	if snapshot.Hit {
		t.Fatal("Get() after recovery reported a hit for a fresh generation")
	}
	if snapshot.Token != "gen-b" {
		t.Fatalf("token = %q, want the recovery generation", snapshot.Token)
	}

	if err := cache.Set(context.Background(), defaultQuery(), service.ListSnapshot{
		Result: pageOf(domain.Task{ID: "id-1", Title: "fresh", Status: domain.StatusTodo}),
		Token:  snapshot.Token,
	}); err != nil {
		t.Fatalf("Set() after recovery: unexpected error: %v", err)
	}
	hit, err := cache.Get(context.Background(), defaultQuery())
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
		if _, err := cache.Get(context.Background(), defaultQuery()); err == nil {
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
	ctx := context.Background()
	query := defaultQuery()

	// A GET misses and reads the old generation.
	stale, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if stale.Hit {
		t.Fatal("first Get() unexpectedly hit")
	}

	// A mutation commits and invalidates while that GET is still running.
	if err := cache.Invalidate(ctx); err != nil {
		t.Fatalf("Invalidate() unexpected error: %v", err)
	}

	// The slow GET publishes its old result under the old generation.
	if err := cache.Set(ctx, query, service.ListSnapshot{
		Result: pageOf(domain.Task{ID: "stale", Title: "stale", Status: domain.StatusTodo}),
		Token:  stale.Token,
	}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	// New readers use the new generation and must not see the stale write.
	got, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if got.Hit {
		t.Fatalf("Get() hit stale data after invalidation: %+v", got.Result.Tasks)
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
