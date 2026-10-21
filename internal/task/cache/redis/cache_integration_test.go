package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// newIntegrationCache returns a cache backed by TEST_REDIS_URL with a unique
// key prefix. The suite skips loudly when the variable is absent; CI asserts
// that these tests actually ran.
func newIntegrationCache(t *testing.T, ttl time.Duration) *Cache {
	t.Helper()

	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set; skipping Redis integration test")
	}

	prefix := fmt.Sprintf("task-manager-test:%d", time.Now().UnixNano())
	cache, err := New(Config{URL: url, TTL: ttl, KeyPrefix: prefix})
	if err != nil {
		t.Fatalf("create Redis cache: %v", err)
	}
	t.Cleanup(func() {
		cleanupRedisKeys(t, url, prefix)
		if err := cache.Close(); err != nil {
			t.Logf("close Redis cache: %v", err)
		}
	})
	return cache
}

func cleanupRedisKeys(t *testing.T, url, prefix string) {
	t.Helper()

	options, err := goredis.ParseURL(url)
	if err != nil {
		t.Logf("parse TEST_REDIS_URL for cleanup: %v", err)
		return
	}
	client := goredis.NewClient(options)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	keys, err := client.Keys(ctx, prefix+"*").Result()
	if err != nil {
		t.Logf("list Redis test keys: %v", err)
		return
	}
	if len(keys) > 0 {
		if err := client.Del(ctx, keys...).Err(); err != nil {
			t.Logf("delete Redis test keys: %v", err)
		}
	}
}

func listPage(tasks []domain.Task, total int64, page, pageSize int) service.ListResult {
	pages := 0
	if total > 0 {
		pages = int((total + int64(pageSize) - 1) / int64(pageSize))
	}
	return service.ListResult{Tasks: tasks, Page: page, PageSize: pageSize, Total: total, TotalPages: pages}
}

func TestRedisCacheMissThenHit(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()
	query := service.ListQuery{Page: 1, PageSize: 20}

	miss, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if miss.Hit {
		t.Fatal("first Get() reported a hit on a fresh prefix")
	}

	tasks := []domain.Task{
		{ID: "id-1", Title: "first", Description: "one", Status: domain.StatusTodo, Assignee: "alice"},
		{ID: "id-2", Title: "second", Status: domain.StatusDone},
	}
	result := listPage(tasks, 2, 1, 20)
	if err := cache.Set(ctx, query, service.ListSnapshot{Result: result, Token: miss.Token}); err != nil {
		t.Fatalf("Set(): %v", err)
	}

	hit, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() hit: %v", err)
	}
	if !hit.Hit {
		t.Fatal("Get() missed after Set()")
	}
	if !samePage(hit.Result, result) {
		t.Fatalf("result = %+v, want %+v", hit.Result, result)
	}
}

func TestRedisCacheEmptyPageIsAHit(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()
	query := service.ListQuery{Page: 4, PageSize: 5}

	miss, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if err := cache.Set(ctx, query, service.ListSnapshot{
		Result: listPage([]domain.Task{}, 3, 4, 5), Token: miss.Token,
	}); err != nil {
		t.Fatalf("Set(): %v", err)
	}

	hit, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() hit: %v", err)
	}
	if !hit.Hit {
		t.Fatal("empty page was not treated as a hit")
	}
	if hit.Result.Tasks == nil || len(hit.Result.Tasks) != 0 {
		t.Fatalf("tasks = %#v, want a non-nil empty page", hit.Result.Tasks)
	}
	if hit.Result.Total != 3 || hit.Result.Page != 4 {
		t.Fatalf("result = %+v, want the cached metadata", hit.Result)
	}
}

func TestRedisCacheExpires(t *testing.T) {
	cache := newIntegrationCache(t, 100*time.Millisecond)
	ctx := context.Background()
	query := service.ListQuery{Page: 1, PageSize: 20}

	miss, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if err := cache.Set(ctx, query, service.ListSnapshot{
		Result: listPage([]domain.Task{{ID: "id-1", Title: "expiring", Status: domain.StatusTodo}}, 1, 1, 20),
		Token:  miss.Token,
	}); err != nil {
		t.Fatalf("Set(): %v", err)
	}
	if hit, err := cache.Get(ctx, query); err != nil || !hit.Hit {
		t.Fatalf("Get() before expiry = hit %v, err %v; want a hit", hit.Hit, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cache.Get(ctx, query)
		if err != nil {
			t.Fatalf("Get() after expiry: %v", err)
		}
		if !got.Hit {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("cached page did not expire within 2s")
}

// TestRedisCacheVariantsAreIsolated proves two different queries can be cached
// side by side without answering each other.
func TestRedisCacheVariantsAreIsolated(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	aliceQuery := service.ListQuery{Assignee: "alice", Page: 1, PageSize: 20}
	bobQuery := service.ListQuery{Assignee: "bob", Page: 1, PageSize: 20}

	aliceTasks := []domain.Task{{ID: "a-1", Title: "alice", Assignee: "alice", Status: domain.StatusTodo}}
	bobTasks := []domain.Task{
		{ID: "b-1", Title: "bob one", Assignee: "bob", Status: domain.StatusTodo},
		{ID: "b-2", Title: "bob two", Assignee: "bob", Status: domain.StatusDone},
	}

	for query, tasks := range map[service.ListQuery][]domain.Task{
		aliceQuery: aliceTasks,
		bobQuery:   bobTasks,
	} {
		miss, err := cache.Get(ctx, query)
		if err != nil {
			t.Fatalf("Get(%+v) miss: %v", query, err)
		}
		if err := cache.Set(ctx, query, service.ListSnapshot{
			Result: listPage(tasks, int64(len(tasks)), 1, 20), Token: miss.Token,
		}); err != nil {
			t.Fatalf("Set(%+v): %v", query, err)
		}
	}

	aliceHit, err := cache.Get(ctx, aliceQuery)
	if err != nil {
		t.Fatalf("Get(alice): %v", err)
	}
	if !aliceHit.Hit || len(aliceHit.Result.Tasks) != 1 || aliceHit.Result.Tasks[0].ID != "a-1" {
		t.Fatalf("alice page = %+v, want only alice's task", aliceHit.Result)
	}

	bobHit, err := cache.Get(ctx, bobQuery)
	if err != nil {
		t.Fatalf("Get(bob): %v", err)
	}
	if !bobHit.Hit || bobHit.Result.Total != 2 || bobHit.Result.Tasks[0].ID != "b-1" {
		t.Fatalf("bob page = %+v, want bob's two tasks", bobHit.Result)
	}

	// A page variant of the same filter must not reuse page 1's entry.
	pageTwo, err := cache.Get(ctx, service.ListQuery{Assignee: "bob", Page: 2, PageSize: 1})
	if err != nil {
		t.Fatalf("Get(page 2): %v", err)
	}
	if pageTwo.Hit {
		t.Fatal("page 2 hit page 1's cached entry")
	}
}

func TestRedisCacheInvalidateHidesEveryVariant(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	queries := []service.ListQuery{
		{Page: 1, PageSize: 20},
		{Status: domain.StatusDone, Page: 1, PageSize: 20},
		{Assignee: "alice", Page: 2, PageSize: 5},
	}
	for _, query := range queries {
		miss, err := cache.Get(ctx, query)
		if err != nil {
			t.Fatalf("Get() miss: %v", err)
		}
		if err := cache.Set(ctx, query, service.ListSnapshot{
			Result: listPage([]domain.Task{{ID: "old", Title: "old", Status: domain.StatusDone}}, 1, 1, 20),
			Token:  miss.Token,
		}); err != nil {
			t.Fatalf("Set(): %v", err)
		}
	}

	if err := cache.Invalidate(ctx); err != nil {
		t.Fatalf("Invalidate(): %v", err)
	}

	for _, query := range queries {
		got, err := cache.Get(ctx, query)
		if err != nil {
			t.Fatalf("Get() after invalidation: %v", err)
		}
		if got.Hit {
			t.Fatalf("query %+v still hit after invalidation", query)
		}
	}
}

// TestRedisCacheStaleRefillRace reproduces the dangerous ordering: a reader
// observes generation A, a mutation advances to B, and the reader then stores
// its stale result under A. The next reader must still miss.
func TestRedisCacheStaleRefillRace(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()
	query := service.ListQuery{Page: 1, PageSize: 20}

	stale, err := cache.Get(ctx, query) // generation A, miss
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if stale.Hit {
		t.Fatal("first Get() unexpectedly hit")
	}

	if err := cache.Invalidate(ctx); err != nil { // mutation -> generation B
		t.Fatalf("Invalidate(): %v", err)
	}

	if err := cache.Set(ctx, query, service.ListSnapshot{ // slow reader writes A
		Result: listPage([]domain.Task{{ID: "stale", Title: "stale", Status: domain.StatusTodo}}, 1, 1, 20),
		Token:  stale.Token,
	}); err != nil {
		t.Fatalf("Set() stale snapshot: %v", err)
	}

	got, err := cache.Get(ctx, query) // generation B has no entry
	if err != nil {
		t.Fatalf("Get() after stale refill: %v", err)
	}
	if got.Hit {
		t.Fatalf("stale refill became visible: %+v", got.Result.Tasks)
	}
}

func TestRedisCacheMalformedValueIsMissAndAllowsRefill(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()
	query := service.ListQuery{Page: 1, PageSize: 20}

	miss, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	if err := cache.store.Set(ctx, key, "{not json", time.Minute); err != nil {
		t.Fatalf("write malformed value: %v", err)
	}

	got, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() with malformed JSON: unexpected error %v", err)
	}
	if got.Hit {
		t.Fatal("Get() reported a hit for malformed JSON")
	}
	if got.Token != miss.Token {
		t.Fatalf("token = %q, want %q", got.Token, miss.Token)
	}
	if _, err := cache.store.Get(ctx, key); !errors.Is(err, goredis.Nil) {
		t.Fatalf("poison key still present after recovery: %v", err)
	}

	page := listPage([]domain.Task{{ID: "id-1", Title: "refilled", Status: domain.StatusTodo}}, 1, 1, 20)
	if err := cache.Set(ctx, query, service.ListSnapshot{Result: page, Token: got.Token}); err != nil {
		t.Fatalf("Set() after poison recovery: %v", err)
	}
	hit, err := cache.Get(ctx, query)
	if err != nil || !hit.Hit || hit.Result.Tasks[0].Title != "refilled" {
		t.Fatalf("Get() after refill = hit=%v err=%v result=%+v", hit.Hit, err, hit.Result)
	}
}

func TestRedisCacheWrongVersionIsMissAndAllowsRefill(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()
	query := service.ListQuery{Page: 1, PageSize: 20}

	miss, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	if err := cache.store.Set(ctx, key, `{"version":1,"tasks":[]}`, time.Minute); err != nil {
		t.Fatalf("write wrong-version value: %v", err)
	}

	got, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() with wrong version: unexpected error %v", err)
	}
	if got.Hit {
		t.Fatal("Get() reported a hit for an unsupported version")
	}
	if _, err := cache.store.Get(ctx, key); !errors.Is(err, goredis.Nil) {
		t.Fatalf("wrong-version key still present after recovery: %v", err)
	}
}

func TestRedisCacheInvalidPayloadDoesNotDeleteConcurrentReplacement(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()
	query := service.ListQuery{Page: 1, PageSize: 20}

	miss, err := cache.Get(ctx, query)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	key := cache.listKey(miss.Token, query)
	poison := "{not json"
	if err := cache.store.Set(ctx, key, poison, time.Minute); err != nil {
		t.Fatalf("write poison: %v", err)
	}

	// Simulate the race the Lua script must win: another writer replaced the
	// poison with a valid payload before compare-and-delete ran. Direct CAD
	// against the observed poison must leave the replacement alone.
	validPage := listPage([]domain.Task{{ID: "id-1", Title: "fresh", Status: domain.StatusTodo}}, 1, 1, 20)
	if err := cache.Set(ctx, query, service.ListSnapshot{Result: validPage, Token: miss.Token}); err != nil {
		t.Fatalf("concurrent Set: %v", err)
	}
	deleted, err := cache.store.CompareAndDelete(ctx, key, poison)
	if err != nil {
		t.Fatalf("CompareAndDelete: %v", err)
	}
	if deleted {
		t.Fatal("CompareAndDelete removed a value that no longer matched the poison")
	}
	hit, err := cache.Get(ctx, query)
	if err != nil || !hit.Hit || hit.Result.Tasks[0].Title != "fresh" {
		t.Fatalf("Get() after preserved refill = hit=%v err=%v result=%+v", hit.Hit, err, hit.Result)
	}
}

func TestRedisCacheConcurrentFirstReadsShareGeneration(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	const readers = 20
	tokens := make(chan string, readers)
	errs := make(chan error, readers)

	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot, err := cache.Get(ctx, service.ListQuery{Page: 1, PageSize: 20})
			if err != nil {
				errs <- err
				return
			}
			tokens <- snapshot.Token
		}()
	}
	wg.Wait()
	close(tokens)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Get(): %v", err)
	}

	var first string
	for token := range tokens {
		if token == "" {
			t.Error("concurrent Get() returned an empty generation token")
			continue
		}
		if first == "" {
			first = token
			continue
		}
		if token != first {
			t.Errorf("generation token = %q, want %q across concurrent first reads", token, first)
		}
	}
	if first == "" {
		t.Fatal("no generation token observed")
	}
}
