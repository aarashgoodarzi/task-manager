// Package redis is the Redis driven adapter for the task-list cache. It
// implements service.ListCache and owns the cache key layout, serialization,
// TTLs, and the generation token that keeps a stale refill from becoming
// visible. It knows nothing about HTTP or PostgreSQL.
package redis

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

const (
	// defaultKeyPrefix namespaces every key this service writes. The
	// generation is part of the list key, so invalidating it only has to
	// replace one small value; old list values expire on their own.
	defaultKeyPrefix = "task-manager"

	// cacheSchemaVersion versions the cached payload. A payload with another
	// version is treated as a miss, not decoded speculatively. Version 2 added
	// the pagination metadata, so version-1 entries are ignored rather than
	// decoded with missing fields.
	cacheSchemaVersion = 2

	// generationReadAttempts bounds the initialize-on-first-read loop when
	// several readers race to create the generation key.
	generationReadAttempts = 3

	redisDialTimeout      = 500 * time.Millisecond
	redisOperationTimeout = 500 * time.Millisecond

	// recoveryInterval paces the attempt to restore caching after a failed
	// invalidation. Without it a process whose invalidation failed once would
	// stay degraded until the next mutation or a restart, and a down Redis
	// would add its dial timeout to every read.
	recoveryInterval = 5 * time.Second
)

// errDegraded reports that invalidation failed earlier and the adapter is
// bypassing the cache. Serving from the database is always safe; reading a
// generation that may have missed an invalidation is not.
var errDegraded = errors.New("redis cache: degraded after failed invalidation")

// Config controls the Redis connection and the list entry lifetime.
type Config struct {
	// URL is a go-redis connection URL, for example
	// redis://localhost:6379/0.
	URL string
	// TTL is the lifetime of a cached list. It must be positive. The
	// generation key does not expire.
	TTL time.Duration
	// KeyPrefix overrides the default key namespace. Tests use a unique
	// prefix so they cannot collide with a running service.
	KeyPrefix string
}

// store is the small command surface this adapter needs. Keeping it separate
// from *goredis.Client makes the generation and failure logic testable with a
// deterministic fake; the real implementation is redisStore below.
type store interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	SetNX(ctx context.Context, key, value string) (bool, error)
	Close() error
}

type redisStore struct {
	client *goredis.Client
}

func (s *redisStore) Get(ctx context.Context, key string) (string, error) {
	return s.client.Get(ctx, key).Result()
}

func (s *redisStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *redisStore) SetNX(ctx context.Context, key, value string) (bool, error) {
	return s.client.SetNX(ctx, key, value, 0).Result()
}

func (s *redisStore) Close() error {
	return s.client.Close()
}

// Cache is a fail-open Redis implementation of service.ListCache.
type Cache struct {
	store    store
	ttl      time.Duration
	prefix   string
	newToken func() (string, error)

	// degraded is set when invalidation could not be written. Reads then
	// bypass the cache until an invalidation succeeds again, so a failed
	// mutation cannot leave the process serving an old generation.
	degraded atomic.Bool

	// retryAt is the earliest Unix-nanosecond time for the next recovery
	// probe. It keeps a degraded cache from dialing Redis on every read.
	retryAt atomic.Int64

	// now is the clock used for recovery pacing. It is nil in production and
	// replaced in tests so the cooldown can be advanced deterministically.
	now func() time.Time
}

var _ service.ListCache = (*Cache)(nil)

// New validates cfg and returns a cache. It does not dial Redis: a missing or
// unreachable server must not prevent the API from starting. Connection errors
// surface on the first operation and are handled by the caller's fallback.
func New(cfg Config) (*Cache, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("redis cache: URL is required")
	}
	if cfg.TTL <= 0 {
		return nil, fmt.Errorf("redis cache: TTL must be positive")
	}

	options, err := goredis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("redis cache: invalid URL: %w", err)
	}
	// Cache operations sit on the request path. Bound them tightly and retry
	// rarely so a Redis outage degrades latency instead of hanging requests.
	options.DialTimeout = redisDialTimeout
	options.ReadTimeout = redisOperationTimeout
	options.WriteTimeout = redisOperationTimeout
	options.PoolTimeout = redisOperationTimeout
	options.MaxRetries = 1

	prefix := cfg.KeyPrefix
	if prefix == "" {
		prefix = defaultKeyPrefix
	}

	return &Cache{
		store:    &redisStore{client: goredis.NewClient(options)},
		ttl:      cfg.TTL,
		prefix:   prefix,
		newToken: newGenerationToken,
	}, nil
}

// Close releases the Redis client. It is safe to call on a nil receiver.
func (c *Cache) Close() error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.Close()
}

// Get returns the cached page for one query and the generation it belongs to.
// A miss still carries the observed generation so the following Set cannot
// write into a newer generation. A degraded cache always reports an error,
// which makes the service fall back to PostgreSQL.
func (c *Cache) Get(ctx context.Context, query service.ListQuery) (service.ListSnapshot, error) {
	if c.degraded.Load() && !c.tryRecover(ctx) {
		return service.ListSnapshot{}, errDegraded
	}

	token, err := c.currentToken(ctx)
	if err != nil {
		return service.ListSnapshot{}, err
	}

	raw, err := c.store.Get(ctx, c.listKey(token, query))
	if errors.Is(err, goredis.Nil) {
		return service.ListSnapshot{Token: token}, nil
	}
	if err != nil {
		return service.ListSnapshot{Token: token}, fmt.Errorf("redis cache: read list: %w", err)
	}

	var payload cachedList
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return service.ListSnapshot{Token: token}, fmt.Errorf("redis cache: decode list: %w", err)
	}
	if payload.Version != cacheSchemaVersion {
		return service.ListSnapshot{Token: token}, fmt.Errorf(
			"redis cache: unsupported payload version %d", payload.Version)
	}

	tasks := payload.Tasks
	if tasks == nil {
		// A cached empty page is valid and must remain an empty slice, not a
		// nil slice that a later encoder could render as null.
		tasks = []domain.Task{}
	}

	return service.ListSnapshot{
		Result: service.ListResult{
			Tasks:      tasks,
			Page:       payload.Page,
			PageSize:   payload.PageSize,
			Total:      payload.Total,
			TotalPages: payload.TotalPages,
		},
		Token: token,
		Hit:   true,
	}, nil
}

// Set stores one query's page under the generation it was read from. If a
// mutation replaced the generation after the read, this write is orphaned and
// expires with its TTL instead of becoming the next reader's answer.
func (c *Cache) Set(ctx context.Context, query service.ListQuery, snapshot service.ListSnapshot) error {
	if snapshot.Token == "" {
		return fmt.Errorf("redis cache: store list: missing generation token")
	}
	if c.degraded.Load() {
		return errDegraded
	}

	payload, err := json.Marshal(cachedList{
		Version:    cacheSchemaVersion,
		Tasks:      snapshot.Result.Tasks,
		Page:       snapshot.Result.Page,
		PageSize:   snapshot.Result.PageSize,
		Total:      snapshot.Result.Total,
		TotalPages: snapshot.Result.TotalPages,
	})
	if err != nil {
		return fmt.Errorf("redis cache: encode list: %w", err)
	}

	if err := c.store.Set(ctx, c.listKey(snapshot.Token, query), string(payload), c.ttl); err != nil {
		return fmt.Errorf("redis cache: store list: %w", err)
	}
	return nil
}

// Invalidate starts a new generation. The atomic SET replaces the token every
// subsequent reader observes; the previous list remains in Redis only until
// its TTL expires and can no longer be addressed through the generation key.
//
// A failed SET marks the adapter degraded. This is deliberately fail-closed for
// reads: the process must not keep serving a generation that may have missed a
// committed mutation. A later successful invalidation clears the state.
func (c *Cache) Invalidate(ctx context.Context) error {
	token, err := c.newToken()
	if err != nil {
		c.markDegraded()
		return fmt.Errorf("redis cache: create generation token: %w", err)
	}

	if err := c.store.Set(ctx, c.generationKey(), token, 0); err != nil {
		c.markDegraded()
		return fmt.Errorf("redis cache: invalidate list: %w", err)
	}

	c.degraded.Store(false)
	return nil
}

// markDegraded disables cache reads and schedules the first recovery probe, so
// the read that just failed cannot immediately dial Redis again.
func (c *Cache) markDegraded() {
	c.degraded.Store(true)
	c.retryAt.Store(c.clock().Add(recoveryInterval).UnixNano())
}

// tryRecover attempts to restore caching after a failed invalidation. It runs at
// most once per recoveryInterval across concurrent readers, and it recovers by
// starting a new generation: the cached list may have missed a committed
// mutation, so it is discarded rather than trusted. Until the probe succeeds,
// reads keep bypassing the cache and the service keeps serving PostgreSQL.
func (c *Cache) tryRecover(ctx context.Context) bool {
	now := c.clock()
	next := c.retryAt.Load()
	if now.UnixNano() < next {
		return false
	}
	if !c.retryAt.CompareAndSwap(next, now.Add(recoveryInterval).UnixNano()) {
		return false
	}
	return c.Invalidate(ctx) == nil
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// generationKey is the small mutable value that names the active list entry.
// It has no TTL: a new process can always read or create it. If Redis evicts
// it, currentToken creates a fresh generation and old list keys are simply
// never addressed again.
func (c *Cache) generationKey() string {
	return c.prefix + ":tasks:list:generation"
}

// listKey names one query variant inside a generation. Every mutation replaces
// the generation, so all variants become unreachable together and expire on
// their own; there is no variant index to maintain or to get wrong.
func (c *Cache) listKey(token string, query service.ListQuery) string {
	return c.prefix + ":tasks:list:" + token + ":" + variantKey(query)
}

// variantKey identifies a normalized query. The service normalizes before
// calling the adapter, so equivalent requests share one entry. The query is
// hashed rather than concatenated so no assignee value can mimic another
// field's delimiter, and the stored keys stay a fixed length.
func variantKey(query service.ListQuery) string {
	canonical := fmt.Sprintf("status=%s\x00assignee=%s\x00page=%d\x00page_size=%d",
		query.Status, query.Assignee, query.Page, query.PageSize)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// currentToken reads the generation, initializing it on first use. The
// SetNX-then-reread loop handles concurrent first readers without a lock.
func (c *Cache) currentToken(ctx context.Context) (string, error) {
	for attempt := 0; attempt < generationReadAttempts; attempt++ {
		token, err := c.store.Get(ctx, c.generationKey())
		if err == nil {
			if token == "" {
				return "", fmt.Errorf("redis cache: empty generation token")
			}
			return token, nil
		}
		if !errors.Is(err, goredis.Nil) {
			return "", fmt.Errorf("redis cache: read generation: %w", err)
		}

		candidate, err := c.newToken()
		if err != nil {
			return "", fmt.Errorf("redis cache: create generation token: %w", err)
		}
		created, err := c.store.SetNX(ctx, c.generationKey(), candidate)
		if err != nil {
			return "", fmt.Errorf("redis cache: initialize generation: %w", err)
		}
		if created {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("redis cache: generation token was not initialized")
}

// cachedList is the stored JSON envelope. The version makes a future payload
// change a miss rather than an accidental decode of incompatible data.
type cachedList struct {
	Version    int           `json:"version"`
	Tasks      []domain.Task `json:"tasks"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Total      int64         `json:"total"`
	TotalPages int           `json:"total_pages"`
}

// newGenerationToken returns 128 random bits as lowercase hex. The value is a
// key component, not a secret; randomness only needs to make collisions
// negligible.
func newGenerationToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
