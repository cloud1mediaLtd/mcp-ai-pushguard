package notifier

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pushguard/pushguard/internal/core"
)

// DefaultRedisEventsChannel is the pub/sub channel PushGuard publishes
// finished-job events to. Anything can SUBSCRIBE to it — a future local web
// dashboard for live updates, another script, a second daemon — without
// needing to poll the daemon's HTTP API or share its Unix socket.
const DefaultRedisEventsChannel = "pushguard:events"

// RedisNotifier publishes a job's final state to a Redis pub/sub channel
// whenever it finishes. It is a Notifier like MacOSNotifier and
// TerminalBellNotifier — MultiNotifier fans out to all of them — so this is
// additive, not a replacement for anything.
type RedisNotifier struct {
	client  *redis.Client
	channel string
}

// NewRedisNotifier connects to addr and pings it once with a short timeout.
// It returns an error instead of a notifier that silently fails on every job
// if Redis isn't reachable at construction time — callers are expected to
// skip adding it to the MultiNotifier chain in that case, matching the same
// graceful-degradation pattern as the daemon's own auto-start/auto-fallback
// behavior. Redis being reachable now doesn't guarantee it stays that way;
// callers should still treat Notify errors as non-fatal the way every other
// Notifier's errors already are.
func NewRedisNotifier(addr, channel string) (*RedisNotifier, error) {
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	if channel == "" {
		channel = DefaultRedisEventsChannel
	}

	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: 2 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}

	return &RedisNotifier{client: client, channel: channel}, nil
}

// Notify publishes the job's JSON encoding to the configured channel.
func (r *RedisNotifier) Notify(job *core.Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return r.client.Publish(ctx, r.channel, data).Err()
}

// Close releases the underlying Redis connection. Callers should defer this
// alongside engine.Stop() during daemon shutdown.
func (r *RedisNotifier) Close() error {
	return r.client.Close()
}
