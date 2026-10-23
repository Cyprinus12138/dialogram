package eval

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPCompleter_RetriesTransientThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"exhausted"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := &HTTPCompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxAttempts: 3, Backoff: time.Millisecond}
	out, err := c.Complete(context.Background(), "sys", "usr")
	if err != nil || out != "ok" || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("out=%q err=%v calls=%d", out, err, calls)
	}
}

func TestHTTPCompleter_PersistentTransientAndPermanent(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := &HTTPCompleter{BaseURL: srv.URL, APIKey: "k", MaxAttempts: 3, Backoff: time.Millisecond}
	_, err := c.Complete(context.Background(), "sys", "usr")
	var te *TransientError
	if !errors.As(err, &te) || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}

	calls = 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	c = &HTTPCompleter{BaseURL: bad.URL, APIKey: "bad", MaxAttempts: 3, Backoff: time.Millisecond}
	_, err = c.Complete(context.Background(), "sys", "usr")
	if err == nil || errors.As(err, &te) || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("401 must surface immediately and untransient: err=%v calls=%d", err, calls)
	}
	if _, err := (&HTTPCompleter{}).Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("missing API key must error")
	}
}
