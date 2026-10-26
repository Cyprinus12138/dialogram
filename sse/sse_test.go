package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/streambuf"
	"github.com/Cyprinus12138/dialogram/tool"
)

type frame struct {
	ID    int
	Event string
	Data  agent.StreamEvent
	Raw   string
}

// readFrames parses an SSE body until EOF.
func readFrames(t *testing.T, body *bufio.Reader) []frame {
	t.Helper()
	var out []frame
	var cur frame
	for {
		line, err := body.ReadString('\n')
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID, _ = atoi(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Raw = strings.TrimPrefix(line, "data: ")
			_ = json.Unmarshal([]byte(cur.Raw), &cur.Data)
		case line == "":
			if cur.Event != "" {
				out = append(out, cur)
			}
			cur = frame{}
		}
		if err != nil {
			return out
		}
	}
}

func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("nan")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func post(t *testing.T, url, body string) (*http.Response, []frame) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp, readFrames(t, bufio.NewReader(resp.Body))
}

func types(fs []frame) string {
	var ts []string
	for _, f := range fs {
		ts = append(ts, f.Event)
	}
	return strings.Join(ts, ",")
}

func newTestHandler(t *testing.T, produce Producer, opts Options) (*Handler, *httptest.Server, *streambuf.MemStore) {
	t.Helper()
	store := streambuf.NewMemStore(time.Minute)
	t.Cleanup(func() { store.Close() })
	if opts.KeepAlive == 0 {
		opts.KeepAlive = 50 * time.Millisecond
	}
	h := NewHandler(store, produce, opts)
	mux := http.NewServeMux()
	mux.Handle("/chat/stream", h)
	mux.Handle("/chat/cancel", h.CancelHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv, store
}

func TestFirstConnect_StreamsAndFinishes(t *testing.T) {
	var finished Result
	var mu sync.Mutex
	produce := func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		if string(body) != `{"message":"hi"}` {
			t.Errorf("body not forwarded: %s", body)
		}
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "hel"})
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "lo"})
		emit(agent.StreamEvent{Type: agent.EventTurn, Turn: &agent.Turn{Role: agent.RoleAssistant}})
		emit(agent.StreamEvent{Type: agent.EventUsage, InputTokens: 10, OutputTokens: 2})
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}
	_, srv, _ := newTestHandler(t, produce, Options{OnFinish: func(_ context.Context, r Result) {
		mu.Lock()
		finished = r
		mu.Unlock()
	}})
	resp, fs := post(t, srv.URL+"/chat/stream?message_id=m1", `{"message":"hi"}`)
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Message-Id") != "m1" {
		t.Fatalf("headers: %v", resp.Header)
	}
	if got := types(fs); got != "meta,text,text,usage,done" {
		t.Fatalf("frames = %s", got)
	}
	if fs[0].Data.MessageID != "m1" || fs[1].ID != 2 || fs[4].ID != 5 {
		t.Fatalf("ids/meta wrong: %+v", fs)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if finished.Status != streambuf.StatusDone || len(finished.Turns) != 1 || finished.Usage.InputTokens != 10 || !finished.EmittedText {
		t.Fatalf("OnFinish result wrong: %+v", finished)
	}
}

// A client that reloads as soon as its stream ends must find what OnFinish
// persists, so the stream may not end before OnFinish returns.
func TestOnFinish_LandsBeforeTheStreamEnds(t *testing.T) {
	produce := func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "hi"})
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}
	var persisted atomic.Bool
	_, srv, _ := newTestHandler(t, produce, Options{OnFinish: func(context.Context, Result) {
		time.Sleep(50 * time.Millisecond)
		persisted.Store(true)
	}})
	_, fs := post(t, srv.URL+"/chat/stream?message_id=m1", `{"message":"hi"}`)
	if got := types(fs); got != "meta,text,done" {
		t.Fatalf("frames = %s", got)
	}
	if !persisted.Load() {
		t.Fatal("the stream ended before OnFinish persisted the run")
	}
}

func TestResume_FromSinceAndPureTail(t *testing.T) {
	release := make(chan struct{})
	produce := func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "a"})
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "b"})
		<-release
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "c"})
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}
	_, srv, _ := newTestHandler(t, produce, Options{})

	// First connection reads the first frames then drops.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/chat/stream?message_id=m2", strings.NewReader(`{"message":"x"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(resp.Body)
	var seen int
	for seen < 3 { // meta, a, b
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "event: ") {
			seen++
		}
	}
	cancel()
	resp.Body.Close()

	// The producer is still alive; release it, then resume from id 3.
	close(release)
	_, fs := post(t, srv.URL+"/chat/stream?message_id=m2&since=3", ``)
	if got := types(fs); got != "text,done" || fs[0].Data.Delta != "c" || fs[0].ID != 4 {
		t.Fatalf("resume frames = %s %+v", got, fs)
	}

	// Pure tail via GET and Last-Event-ID replays the whole buffer.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/chat/stream?message_id=m2", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fs = readFrames(t, bufio.NewReader(resp.Body))
	resp.Body.Close()
	if got := types(fs); got != "text,text,text,done" {
		t.Fatalf("tail frames = %s", got)
	}
	// A retried first connect with the same id does not start a second
	// producer; it tails.
	_, fs = post(t, srv.URL+"/chat/stream?message_id=m2", `{"message":"x"}`)
	if len(fs) != 5 {
		t.Fatalf("retried connect must tail the existing buffer, got %d frames", len(fs))
	}
}

func TestCancel_FinalisesAsDone(t *testing.T) {
	var res Result
	var mu sync.Mutex
	produce := func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "partial"})
		<-ctx.Done()
		return ctx.Err()
	}
	h, srv, _ := newTestHandler(t, produce, Options{OnFinish: func(_ context.Context, r Result) { mu.Lock(); res = r; mu.Unlock() }})
	done := make(chan []frame, 1)
	go func() { _, fs := post(t, srv.URL+"/chat/stream?message_id=m3", `{"m":1}`); done <- fs }()
	time.Sleep(30 * time.Millisecond)
	resp, err := http.Post(srv.URL+"/chat/cancel?message_id=m3", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["cancelled"] != true {
		t.Fatalf("cancel response = %v", body)
	}
	fs := <-done
	last := fs[len(fs)-1]
	if last.Event != agent.EventDone || last.Data.Message != "cancelled" {
		t.Fatalf("cancelled stream must end with done/cancelled, got %+v", last)
	}
	mu.Lock()
	defer mu.Unlock()
	if !res.Cancelled || res.Status != streambuf.StatusDone || res.Err != nil {
		t.Fatalf("OnFinish on cancel wrong: %+v", res)
	}
	if h.Cancel("unknown") {
		t.Fatal("cancel of unknown id must be false")
	}
}

func TestGenUI_ConversionAndPause(t *testing.T) {
	var res Result
	var mu sync.Mutex
	env, _ := json.Marshal(tool.Envelope{ModelObservation: map[string]any{"n": 1}, UIPayload: &tool.UIPayload{Component: "bar_chart", Data: map[string]any{"data": []any{}}},
		RuntimeState: map[string]any{"secret_rows": []int{1, 2}}})
	produce := func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventToolCall, Name: tool.PresentDataToolName, ToolUseID: "p", Input: json.RawMessage(`{"type":"metric_cards"}`)})
		emit(agent.StreamEvent{Type: agent.EventToolResult, Name: tool.PresentDataToolName, ToolUseID: "p", Input: json.RawMessage(`{"type":"metric_cards"}`), Output: `{"displayed":true}`})
		emit(agent.StreamEvent{Type: agent.EventToolCall, Name: "get_weather", ToolUseID: "w"})
		emit(agent.StreamEvent{Type: agent.EventToolResult, Name: "get_weather", ToolUseID: "w", Output: string(env)})
		emit(agent.StreamEvent{Type: agent.EventWaitingForUser, Kind: agent.PauseKindHITL, ToolUseID: "dg_q", Message: "Which city?"})
		return nil
	}
	_, srv, store := newTestHandler(t, produce, Options{OnFinish: func(_ context.Context, r Result) { mu.Lock(); res = r; mu.Unlock() }})
	_, fs := post(t, srv.URL+"/chat/stream?message_id=m4", `{"m":1}`)
	if got := types(fs); got != "meta,gen_ui,tool_call,gen_ui,tool_result,waiting_for_user" {
		t.Fatalf("frames = %s", got)
	}
	if out := fs[4].Data.Output; strings.Contains(out, "secret_rows") || !strings.Contains(out, `"model_observation":{"n":1}`) {
		t.Errorf("the tool_result frame carries the envelope without runtime_state: %s", out)
	}
	if string(fs[1].Data.GenUI) != `{"type":"metric_cards"}` || !strings.Contains(string(fs[3].Data.GenUI), "bar_chart") || fs[3].Data.Name != "get_weather" {
		t.Fatalf("gen_ui payloads wrong: %+v", fs)
	}
	_, status, _ := store.Tail(context.Background(), "m4", 100, 10*time.Millisecond)
	if status != streambuf.StatusWaiting {
		t.Fatalf("buffer status = %q, want waiting", status)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !res.Paused || res.PausedKind != agent.PauseKindHITL || res.PausedToolUseID != "dg_q" || res.PausedPrompt != "Which city?" {
		t.Fatalf("pause not reported: %+v", res)
	}
}

func TestError_IdleTimeout_Panic(t *testing.T) {
	// Producer error → friendly error event + error status.
	_, srv, store := newTestHandler(t, func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		return errors.New("upstream 502: bad gateway")
	}, Options{})
	_, fs := post(t, srv.URL+"/chat/stream?message_id=e1", `{"m":1}`)
	last := fs[len(fs)-1]
	if last.Event != agent.EventError || strings.Contains(last.Data.Message, "502") || last.Data.Message == "" {
		t.Fatalf("error frame must be friendly: %+v", last)
	}
	if _, status, _ := store.Tail(context.Background(), "e1", 100, 10*time.Millisecond); !streambuf.IsError(status) {
		t.Fatalf("status = %q", status)
	}

	// Idle watchdog cancels a silent producer.
	_, srv2, _ := newTestHandler(t, func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		<-ctx.Done()
		return ctx.Err()
	}, Options{IdleTimeout: 60 * time.Millisecond})
	start := time.Now()
	_, fs = post(t, srv2.URL+"/chat/stream?message_id=e2", `{"m":1}`)
	if fs[len(fs)-1].Event != agent.EventError || time.Since(start) > 2*time.Second {
		t.Fatalf("idle producer must be cut short with an error: %+v", fs[len(fs)-1])
	}

	// A panicking producer still leaves a terminal error event.
	_, srv3, _ := newTestHandler(t, func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		panic("boom")
	}, Options{})
	_, fs = post(t, srv3.URL+"/chat/stream?message_id=e3", `{"m":1}`)
	if fs[len(fs)-1].Event != agent.EventError {
		t.Fatalf("panic must surface as an error frame: %+v", fs)
	}
}

func TestLaunch_ProgrammaticStartThenTail(t *testing.T) {
	h, srv, _ := newTestHandler(t, func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: string(body)})
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}, Options{})
	acquired, err := h.Launch(context.Background(), "l1", []byte("continuation"))
	if err != nil || !acquired {
		t.Fatalf("Launch: acquired=%v err=%v", acquired, err)
	}
	if again, _ := h.Launch(context.Background(), "l1", nil); again {
		t.Fatal("second Launch must not acquire")
	}
	_, fs := post(t, srv.URL+"/chat/stream?message_id=l1", ``)
	if got := types(fs); got != "meta,text,done" || fs[1].Data.Delta != "continuation" {
		t.Fatalf("tail after Launch = %s %+v", got, fs)
	}
}

func TestRequestValidation(t *testing.T) {
	_, srv, _ := newTestHandler(t, func(context.Context, string, []byte, agent.EmitFn) error { return nil }, Options{MaxBodyBytes: 8})
	resp, _ := http.Get(srv.URL + "/chat/stream")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET without message_id = %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/chat/stream", "application/json", strings.NewReader(`{"message":"too long"}`))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/chat/stream", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/chat/cancel?message_id=x")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET cancel = %d", resp.StatusCode)
	}
	if DefaultFriendlyError(nil) != "" || !strings.Contains(DefaultFriendlyError(context.DeadlineExceeded), "too long") ||
		!strings.Contains(DefaultFriendlyError(errors.New("429 rate limit")), "busy") || len(NewMessageID()) != 32 {
		t.Fatal("helpers wrong")
	}
}
