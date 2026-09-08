package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/pushguard/pushguard/pkg/api"
)

func fakeClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	proxyURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(transport.CloseIdleConnections)
	return &Client{httpClient: &http.Client{Transport: transport}}
}

func TestIncompatibleDaemonRejectsBeforeSubmission(t *testing.T) {
	posts := 0
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.Write([]byte(`{"status":"ok"}`))
	})
	_, _, err := c.Enqueue(api.EnqueueRequest{RepoPath: "/unused"})
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("got %v", err)
	}
	if err := c.EnsureDaemon(1); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("ensure returned %v", err)
	}
	if posts != 0 {
		t.Fatal("submitted to incompatible daemon")
	}
}

func TestAwaitDeadlineCancelsInFlightRequest(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Await(ctx, "pg_test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("request outlived wait deadline")
	}
}

func TestGetJobRejectsMissingPayloadAndServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write([]byte(`{}`)) })
			if _, err := c.GetJob("pg_test"); err == nil {
				t.Fatal("accepted missing verdict")
			}
		})
	}
}

func TestSubmissionRequiresMatchingProtocolAndBuild(t *testing.T) {
	for _, protocol := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			posted := false
			c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprintf(w, `{"protocol_version":%d,"build":"client-build","capabilities":["immutable-refs","explicit-snapshot","timeout-seconds","no-auto-push"]}`, protocol)
					return
				}
				posted = true
				if r.Header.Get(api.ProtocolHeader) != "2" {
					t.Error("missing submission protocol header")
				}
				w.WriteHeader(http.StatusAccepted)
				w.Write([]byte(`{"job":{"id":"pg_accepted"},"position":1}`))
			})
			c.build = "client-build"
			_, _, err := c.Enqueue(api.EnqueueRequest{})
			if protocol == 2 {
				if err != nil || !posted {
					t.Fatalf("compatible protocol rejected: %v", err)
				}
			} else if !errors.Is(err, ErrIncompatible) || posted {
				t.Fatalf("incompatible protocol submitted: %v", err)
			}
		})
	}
}

func TestSubmissionRefusesBuildMismatchBeforePost(t *testing.T) {
	posted := false
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		w.Write([]byte(`{"protocol_version":2,"build":"daemon-build","capabilities":["immutable-refs","explicit-snapshot","timeout-seconds","no-auto-push"]}`))
	})
	c.build = "client-build"
	_, _, err := c.Enqueue(api.EnqueueRequest{})
	if !errors.Is(err, ErrIncompatible) || posted {
		t.Fatalf("mismatch submitted: %v", err)
	}
}

func TestEnqueueSurfacesReusedActiveJob(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"protocol_version":2,"build":"daemon","capabilities":["immutable-refs","explicit-snapshot","timeout-seconds","no-auto-push"]}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"job":{"id":"pg_existing","reused":true,"dedupe_key":"gate:sha"},"position":0}`))
	})
	job, position, err := c.Enqueue(api.EnqueueRequest{DedupeKey: "gate:sha"})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "pg_existing" || !job.Reused || job.DedupeKey != "gate:sha" || position != 0 {
		t.Fatalf("submission = %+v at %d", job, position)
	}
}
