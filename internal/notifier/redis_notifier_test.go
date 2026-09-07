package notifier

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pushguard/pushguard/internal/core"
)

const testRedisAddr = "127.0.0.1:6379"

func requireTestRedis(t *testing.T) {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: testRedisAddr, DialTimeout: 500 * time.Millisecond})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("no Redis reachable at %s, skipping: %v", testRedisAddr, err)
	}
}

func TestNewRedisNotifier_FailsFastWhenUnreachable(t *testing.T) {
	// A port nothing listens on: this must return an error quickly, not hang
	// or silently produce a notifier that fails on every real Notify call.
	start := time.Now()
	_, err := NewRedisNotifier("127.0.0.1:1", "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error connecting to a port nothing listens on")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("expected a fast failure, took %s", elapsed)
	}
}

func TestRedisNotifier_PublishesJobJSON(t *testing.T) {
	requireTestRedis(t)

	channel := "pushguard:events:test"
	sub := redis.NewClient(&redis.Options{Addr: testRedisAddr})
	defer sub.Close()
	pubsub := sub.Subscribe(context.Background(), channel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(context.Background()); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	rn, err := NewRedisNotifier(testRedisAddr, channel)
	if err != nil {
		t.Fatalf("NewRedisNotifier failed against a reachable Redis: %v", err)
	}
	defer rn.Close()

	job := &core.Job{ID: "pg_test123", Name: "unit-test", Status: core.StatusPassed}
	if err := rn.Notify(job); err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	select {
	case msg := <-pubsub.Channel():
		var got core.Job
		if err := json.Unmarshal([]byte(msg.Payload), &got); err != nil {
			t.Fatalf("published payload wasn't valid job JSON: %v", err)
		}
		if got.ID != job.ID {
			t.Fatalf("expected job ID %q, got %q", job.ID, got.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the published event")
	}
}
