package streambuf

import (
	"context"
	"os"
	"testing"
	"time"
)

// storeContract exercises the Store contract against any implementation.
func storeContract(t *testing.T, s Store, id string) {
	t.Helper()
	ctx := context.Background()

	// Exactly one Start wins.
	ok, err := s.Start(ctx, id)
	if err != nil || !ok {
		t.Fatalf("first Start: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.Start(ctx, id); ok {
		t.Fatal("second Start must not acquire")
	}

	// Tail with nothing yet times out with no status.
	evs, status, err := s.Tail(ctx, id, 0, 30*time.Millisecond)
	if err != nil || len(evs) != 0 || status != "" {
		t.Fatalf("empty tail: evs=%d status=%q err=%v", len(evs), status, err)
	}

	// Append wakes a blocked tail.
	woke := make(chan []Event, 1)
	go func() {
		evs, _, _ := s.Tail(ctx, id, 0, 2*time.Second)
		woke <- evs
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.Append(ctx, id, `{"type":"text","delta":"a"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case evs := <-woke:
		if len(evs) != 1 || evs[0].Seq != 1 {
			t.Fatalf("woken tail got %+v", evs)
		}
	case <-time.After(time.Second):
		t.Fatal("tail was not woken by Append")
	}

	// Resume from since: only newer events, seqs continue.
	_ = s.Append(ctx, id, `{"type":"text","delta":"b"}`)
	_ = s.Append(ctx, id, `{"type":"text","delta":"c"}`)
	evs, status, _ = s.Tail(ctx, id, 1, time.Second)
	if len(evs) != 2 || evs[0].Seq != 2 || evs[1].Seq != 3 || status != "" {
		t.Fatalf("resume tail: %+v status=%q", evs, status)
	}

	// Finish is terminal; first status wins; tail past the end reports it.
	if err := s.Finish(ctx, id, StatusDone); err != nil {
		t.Fatal(err)
	}
	_ = s.Finish(ctx, id, ErrorStatus("late"))
	evs, status, _ = s.Tail(ctx, id, 3, time.Second)
	if len(evs) != 0 || status != StatusDone {
		t.Fatalf("finished tail: evs=%d status=%q", len(evs), status)
	}
	// A tail that still has events to deliver returns them with the status.
	evs, status, _ = s.Tail(ctx, id, 0, time.Second)
	if len(evs) != 3 || status != StatusDone {
		t.Fatalf("full replay: evs=%d status=%q", len(evs), status)
	}

	// Context cancellation aborts a blocked tail.
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, _, err := s.Tail(cctx, id+"-other", 0, time.Second); err == nil {
		t.Fatal("cancelled tail must return an error")
	}
}

func TestMemStore_Contract(t *testing.T) {
	s := NewMemStore(time.Minute)
	defer s.Close()
	storeContract(t, s, "m1")
	if err := s.Append(context.Background(), "unknown", "x"); err == nil {
		t.Fatal("append to unknown id must error")
	}
	if err := s.Finish(context.Background(), "unknown", StatusDone); err == nil {
		t.Fatal("finish of unknown id must error")
	}
}

func TestMemStore_Reap(t *testing.T) {
	s := NewMemStore(10 * time.Millisecond)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.Start(ctx, "done")
	_ = s.Append(ctx, "done", "x")
	_ = s.Finish(ctx, "done", StatusDone)
	_, _ = s.Start(ctx, "live")
	_ = s.Append(ctx, "live", "x")
	time.Sleep(25 * time.Millisecond)
	if n := s.Reap(time.Now()); n != 1 || s.Len() != 1 {
		t.Fatalf("reap must drop only the finished+expired buffer: n=%d len=%d", n, s.Len())
	}
	// Unfinished buffers are never reaped, however old.
	if n := s.Reap(time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("live buffer must survive, reaped %d", n)
	}
}

func TestStatusHelpers(t *testing.T) {
	if !IsError(ErrorStatus("boom")) || IsError(StatusDone) || IsError(StatusWaiting) {
		t.Fatal("status helpers wrong")
	}
}

// Integration: set REDIS_URL to run the same contract against Redis.
func TestRedisStore_Contract(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	s, err := NewRedisStore(context.Background(), RedisOptions{URL: url, KeyPrefix: "dialogram-test:stream:", TTL: time.Minute})
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	defer s.Close()
	storeContract(t, s, "r-"+time.Now().Format("150405.000"))
}

func TestRedisStore_OptionsValidation(t *testing.T) {
	if _, err := NewRedisStore(context.Background(), RedisOptions{}); err == nil {
		t.Fatal("missing URL and Client must error")
	}
	if _, err := NewRedisStore(context.Background(), RedisOptions{URL: "://bad"}); err == nil {
		t.Fatal("bad URL must error")
	}
}
