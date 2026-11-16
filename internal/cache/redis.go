// Package cache is the Redis adapter behind quotation.Cache.
package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type Redis struct {
	client  *redis.Client
	timeout time.Duration
}

// NewRedis does not connect: the first command does. Every command is capped by timeout, so an
// unreachable Redis costs the request a bounded wait and then counts as a miss.
func NewRedis(addr string, timeout time.Duration) *Redis {
	return &Redis{
		client: redis.NewClient(&redis.Options{
			Addr:         addr,
			DialTimeout:  timeout,
			ReadTimeout:  timeout,
			WriteTimeout: timeout,
			MaxRetries:   -1,
		}),
		timeout: timeout,
	}
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	value, err := r.client.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return value, true, nil
}

// Set always writes with an expiry: Redis itself discards the entry when the TTL ends.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *Redis) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Close() error { return r.client.Close() }
