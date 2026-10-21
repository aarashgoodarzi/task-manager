package redis

import (
	"context"
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

func TestRedisCacheMissThenHit(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	miss, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if miss.Hit {
		t.Fatal("first Get() reported a hit on a fresh prefix")
	}

	tasks := []domain.Task{
		{ID: "id-1", Title: "first", Description: "one", Status: domain.StatusTodo},
		{ID: "id-2", Title: "second", Status: domain.StatusDone},
	}
	if err := cache.Set(ctx, service.ListSnapshot{Tasks: tasks, Token: miss.Token}); err != nil {
		t.Fatalf("Set(): %v", err)
	}

	hit, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() hit: %v", err)
	}
	if !hit.Hit {
		t.Fatal("Get() missed after Set()")
	}
	if len(hit.Tasks) != 2 || hit.Tasks[0] != tasks[0] || hit.Tasks[1] != tasks[1] {
		t.Fatalf("tasks = %+v, want %+v", hit.Tasks, tasks)
	}
}

func TestRedisCacheEmptyListIsAHit(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	miss, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if err := cache.Set(ctx, service.ListSnapshot{Tasks: []domain.Task{}, Token: miss.Token}); err != nil {
		t.Fatalf("Set(): %v", err)
	}

	hit, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() hit: %v", err)
	}
	if !hit.Hit {
		t.Fatal("empty list was not treated as a hit")
	}
	if hit.Tasks == nil || len(hit.Tasks) != 0 {
		t.Fatalf("tasks = %#v, want a non-nil empty list", hit.Tasks)
	}
}

func TestRedisCacheExpires(t *testing.T) {
	cache := newIntegrationCache(t, 100*time.Millisecond)
	ctx := context.Background()

	miss, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if err := cache.Set(ctx, service.ListSnapshot{
		Tasks: []domain.Task{{ID: "id-1", Title: "expiring", Status: domain.StatusTodo}},
		Token: miss.Token,
	}); err != nil {
		t.Fatalf("Set(): %v", err)
	}
	if hit, err := cache.Get(ctx); err != nil || !hit.Hit {
		t.Fatalf("Get() before expiry = hit %v, err %v; want a hit", hit.Hit, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cache.Get(ctx)
		if err != nil {
			t.Fatalf("Get() after expiry: %v", err)
		}
		if !got.Hit {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("cached list did not expire within 2s")
}

func TestRedisCacheInvalidateChangesGeneration(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	first, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if err := cache.Set(ctx, service.ListSnapshot{
		Tasks: []domain.Task{{ID: "id-1", Title: "old", Status: domain.StatusTodo}},
		Token: first.Token,
	}); err != nil {
		t.Fatalf("Set(): %v", err)
	}
	if err := cache.Invalidate(ctx); err != nil {
		t.Fatalf("Invalidate(): %v", err)
	}

	second, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() after invalidation: %v", err)
	}
	if second.Hit {
		t.Fatalf("Get() after invalidation hit the old list: %+v", second.Tasks)
	}
	if second.Token == first.Token {
		t.Fatalf("generation stayed %q after invalidation", first.Token)
	}
}

// TestRedisCacheStaleRefillRace reproduces the dangerous ordering: a reader
// observes generation A, a mutation advances to B, and the reader then stores
// its stale result under A. The next reader must still miss.
func TestRedisCacheStaleRefillRace(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	stale, err := cache.Get(ctx) // generation A, miss
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if stale.Hit {
		t.Fatal("first Get() unexpectedly hit")
	}

	if err := cache.Invalidate(ctx); err != nil { // mutation -> generation B
		t.Fatalf("Invalidate(): %v", err)
	}

	if err := cache.Set(ctx, service.ListSnapshot{ // slow reader writes A
		Tasks: []domain.Task{{ID: "stale", Title: "stale", Status: domain.StatusTodo}},
		Token: stale.Token,
	}); err != nil {
		t.Fatalf("Set() stale snapshot: %v", err)
	}

	got, err := cache.Get(ctx) // generation B has no list
	if err != nil {
		t.Fatalf("Get() after stale refill: %v", err)
	}
	if got.Hit {
		t.Fatalf("stale refill became visible: %+v", got.Tasks)
	}
}

func TestRedisCacheMalformedValueFallsBackWithError(t *testing.T) {
	cache := newIntegrationCache(t, time.Minute)
	ctx := context.Background()

	miss, err := cache.Get(ctx)
	if err != nil {
		t.Fatalf("Get() miss: %v", err)
	}
	if err := cache.store.Set(ctx, cache.listKey(miss.Token), "{not json", time.Minute); err != nil {
		t.Fatalf("write malformed value: %v", err)
	}

	if _, err := cache.Get(ctx); err == nil {
		t.Fatal("Get() with malformed JSON: want error, got nil")
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
			snapshot, err := cache.Get(ctx)
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
