package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnqueueRequestJSONRoundTrip(t *testing.T) {
	in := EnqueueRequest{RepoPath: "/repo", Command: []string{"go test ./..."}, Timeout: 90 * time.Second, Snapshot: true, DedupeKey: "gate:abc"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out EnqueueRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Timeout != in.Timeout || out.RepoPath != in.RepoPath || !out.Snapshot || out.DedupeKey != in.DedupeKey {
		t.Fatalf("round trip = %#v", out)
	}
}

func TestEnqueueRequestRejectsFractionalTimeout(t *testing.T) {
	if _, err := json.Marshal(EnqueueRequest{Timeout: time.Millisecond}); err == nil {
		t.Fatal("expected error")
	}
}

func TestJobIsFinished(t *testing.T) {
	for _, status := range []JobStatus{StatusQueued, StatusRunning, StatusPassed, StatusFailed, StatusCancelled} {
		got := (Job{Status: status}).IsFinished()
		want := status == StatusPassed || status == StatusFailed || status == StatusCancelled
		if got != want {
			t.Errorf("%s: got %v, want %v", status, got, want)
		}
	}
}

func TestJobDurationAndHealthCapability(t *testing.T) {
	started := time.Now().Add(-2 * time.Second)
	finished := started.Add(1500 * time.Millisecond)
	if got := (Job{StartedAt: &started, FinishedAt: &finished}).DurationString(); got != "1.5s" {
		t.Fatalf("duration = %s", got)
	}
	health := Health{Capabilities: []string{"immutable-refs"}}
	if !health.HasCapability("immutable-refs") || health.HasCapability("missing") {
		t.Fatal("capability lookup failed")
	}
}

func TestEnqueueRequestRejectsInvalidWireTimeout(t *testing.T) {
	var req EnqueueRequest
	if err := json.Unmarshal([]byte(`{"timeout_seconds":-1}`), &req); err == nil {
		t.Fatal("expected negative timeout error")
	}
}
