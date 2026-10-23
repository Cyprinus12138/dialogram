package streambuf

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultKeyPrefix is the prefix of every Redis key RedisStore writes.
const DefaultKeyPrefix = "dialogram:stream:"

// RedisOptions configure a RedisStore. Exactly one of URL or Client is
// required.
type RedisOptions struct {
	// URL is a redis:// URL ("redis://:pass@host:6379/0"). Ignored when
	// Client is set.
	URL string
	// Client is an existing client to reuse (not closed by Close).
	Client *redis.Client
	// KeyPrefix defaults to DefaultKeyPrefix. Set a per-environment prefix
	// when several deployments share one Redis.
	KeyPrefix string
	// TTL bounds every key; defaults to DefaultTTL.
	TTL time.Duration
}

// RedisStore uses Redis to persist stream buffers. Safe across multiple
// backend instances: Start uses SETNX for atomic ownership, Append uses
// RPUSH, and Tail blocks on a pub/sub channel for wakeups.
//
// Keys (all under KeyPrefix):
//
//	<prefix><id>:events  LIST  — append-only events
//	<prefix><id>:meta    HASH  — status (done / waiting / error:…)
//	<prefix><id>:owner   STR   — SETNX sentinel for Start
//	<prefix><id>:notify  SUB   — pub/sub channel for Tail wakeups
type RedisStore struct {
	client *redis.Client
	owned  bool
	prefix string
	ttl    time.Duration
}

// NewRedisStore builds a store from opts, pinging once to fail fast on
// misconfiguration.
func NewRedisStore(ctx context.Context, opts RedisOptions) (*RedisStore, error) {
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = DefaultKeyPrefix
	}
	cli := opts.Client
	owned := false
	if cli == nil {
		if opts.URL == "" {
			return nil, errors.New("streambuf: RedisOptions needs URL or Client")
		}
		o, err := redis.ParseURL(opts.URL)
		if err != nil {
			return nil, fmt.Errorf("streambuf: parse redis url: %w", err)
		}
		cli = redis.NewClient(o)
		owned = true
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := cli.Ping(pctx).Err(); err != nil {
		if owned {
			_ = cli.Close()
		}
		return nil, fmt.Errorf("streambuf: ping redis: %w", err)
	}
	return &RedisStore{client: cli, owned: owned, prefix: opts.KeyPrefix, ttl: opts.TTL}, nil
}

// Close releases the connection pool when the store created it.
func (r *RedisStore) Close() error {
	if r.owned {
		return r.client.Close()
	}
	return nil
}

func (r *RedisStore) keyEvents(id string) string { return r.prefix + id + ":events" }
func (r *RedisStore) keyMeta(id string) string   { return r.prefix + id + ":meta" }
func (r *RedisStore) keyOwner(id string) string  { return r.prefix + id + ":owner" }
func (r *RedisStore) keyNotify(id string) string { return r.prefix + id + ":notify" }

// Start implements Store.
func (r *RedisStore) Start(ctx context.Context, msgID string) (bool, error) {
	ok, err := r.client.SetNX(ctx, r.keyOwner(msgID), "1", r.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx owner: %w", err)
	}
	return ok, nil
}

// Append implements Store.
func (r *RedisStore) Append(ctx context.Context, msgID string, data string) error {
	pipe := r.client.Pipeline()
	pipe.RPush(ctx, r.keyEvents(msgID), data)
	pipe.Expire(ctx, r.keyEvents(msgID), r.ttl)
	pipe.Expire(ctx, r.keyMeta(msgID), r.ttl)
	pipe.Expire(ctx, r.keyOwner(msgID), r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis rpush: %w", err)
	}
	// Publish is fire-and-forget: a dropped notification is fine because
	// Tail re-checks state after each wakeup and also times out.
	_ = r.client.Publish(ctx, r.keyNotify(msgID), "e").Err()
	return nil
}

// Finish implements Store.
func (r *RedisStore) Finish(ctx context.Context, msgID string, status string) error {
	pipe := r.client.Pipeline()
	pipe.HSetNX(ctx, r.keyMeta(msgID), "status", status)
	pipe.Expire(ctx, r.keyMeta(msgID), r.ttl)
	pipe.Expire(ctx, r.keyEvents(msgID), r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis finish: %w", err)
	}
	_ = r.client.Publish(ctx, r.keyNotify(msgID), "f").Err()
	return nil
}

// Tail implements Store. It subscribes to the notify channel BEFORE
// reading the list length so an append landing between the two cannot be
// missed.
func (r *RedisStore) Tail(ctx context.Context, msgID string, since int, timeout time.Duration) ([]Event, string, error) {
	if since < 0 {
		since = 0
	}
	sub := r.client.Subscribe(ctx, r.keyNotify(msgID))
	defer func() { _ = sub.Close() }()
	if _, err := sub.Receive(ctx); err != nil {
		return nil, "", fmt.Errorf("redis subscribe: %w", err)
	}
	notifyCh := sub.Channel()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		total, err := r.client.LLen(ctx, r.keyEvents(msgID)).Result()
		if err != nil {
			return nil, "", fmt.Errorf("redis llen: %w", err)
		}
		if int64(since) < total {
			raws, err := r.client.LRange(ctx, r.keyEvents(msgID), int64(since), -1).Result()
			if err != nil {
				return nil, "", fmt.Errorf("redis lrange: %w", err)
			}
			status, _ := r.client.HGet(ctx, r.keyMeta(msgID), "status").Result()
			out := make([]Event, len(raws))
			for i, data := range raws {
				out[i] = Event{Seq: since + i + 1, Data: data}
			}
			return out, status, nil
		}
		status, err := r.client.HGet(ctx, r.keyMeta(msgID), "status").Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, "", fmt.Errorf("redis hget status: %w", err)
		}
		if status != "" {
			return nil, status, nil
		}
		select {
		case <-notifyCh:
		case <-deadline.C:
			return nil, "", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
}
