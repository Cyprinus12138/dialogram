package agui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
)

const runInput = `{"threadId":"t1","runId":"r1","state":{},"messages":[{"id":"u1","role":"user","content":"hi"}],"tools":[],"context":[],"forwardedProps":{}}`

// readFrame reads one "data: …\n\n" frame. ok=false at EOF.
func readFrame(t *testing.T, r *bufio.Reader) (Event, bool) {
	t.Helper()
	var data string
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"):
			t.Fatalf("AG-UI frames carry no %q line", line)
		case line == "" && data != "":
			var ev Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("frame %q: %v", data, err)
			}
			return ev, true
		}
		if err != nil {
			if data != "" {
				t.Fatalf("stream ended inside a frame: %q", data)
			}
			return Event{}, false
		}
	}
}

func readAll(t *testing.T, r io.Reader) []Event {
	t.Helper()
	br := bufio.NewReader(r)
	var out []Event
	for {
		ev, ok := readFrame(t, br)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHandlerSmoke(t *testing.T) {
	var seen RunAgentInput
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error {
		seen = in
		turns, err := MessagesToTurns(in.Messages)
		if err != nil || len(turns) != 1 || turns[0].Blocks[0].Text != "hi" {
			t.Errorf("runner got turns %+v err %v", turns, err)
		}
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "Hello"})
		emit(agent.StreamEvent{Type: agent.EventToolCall, Name: "ping", ToolUseID: "dg_1", Input: raw(`{}`)})
		emit(agent.StreamEvent{Type: agent.EventToolResult, Name: "ping", ToolUseID: "dg_1", Output: `"pong"`})
		emit(agent.StreamEvent{Type: agent.EventText, Delta: " world"})
		emit(agent.StreamEvent{Type: agent.EventUsage, InputTokens: 3, OutputTokens: 2})
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}
	srv := httptest.NewServer(NewHandler(run, Options{NewID: counter("id_")}))
	defer srv.Close()

	resp := post(t, srv.URL, runInput)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != ContentType {
		t.Fatalf("content type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control %q", cc)
	}
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(rawBody)
	if !strings.HasPrefix(body, "data: {") || !strings.HasSuffix(body, "}\n\n") || strings.Contains(body, "\nevent:") {
		t.Fatalf("unexpected framing:\n%s", body)
	}
	events := readAll(t, strings.NewReader(body))
	checkWellFormed(t, events)
	if strings.Count(body, "\n\n") != len(events) {
		t.Fatalf("frame count %d vs events %d", strings.Count(body, "\n\n"), len(events))
	}
	first, last := events[0], events[len(events)-1]
	if first.Type != EventRunStarted || first.ThreadID != "t1" || first.RunID != "r1" {
		t.Fatalf("first: %+v", first)
	}
	if last.Type != EventRunFinished || last.ThreadID != "t1" || last.RunID != "r1" {
		t.Fatalf("last: %+v", last)
	}
	if seen.ThreadID != "t1" || seen.RunID != "r1" || string(seen.State) != "{}" || string(seen.ForwardedProps) != "{}" {
		t.Fatalf("runner input: %+v", seen)
	}
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	// usage arrives while the second message is open; CUSTOM may appear
	// anywhere, and the message only closes when the run ends.
	want := "RUN_STARTED TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT TEXT_MESSAGE_END TOOL_CALL_START TOOL_CALL_ARGS TOOL_CALL_END TOOL_CALL_RESULT TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT CUSTOM TEXT_MESSAGE_END RUN_FINISHED"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("event order:\n got %s\nwant %s", got, want)
	}
}

// TestHandlerFlushesPerEvent proves frames reach the client while the
// Runner is still running: the Runner blocks after its first delta until
// the test has read that delta off the wire.
func TestHandlerFlushesPerEvent(t *testing.T) {
	release := make(chan struct{})
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "first"})
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		emit(agent.StreamEvent{Type: agent.EventDone})
		return nil
	}
	srv := httptest.NewServer(NewHandler(run, Options{}))
	defer srv.Close()

	resp := post(t, srv.URL, runInput)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	got := make(chan Event, 3)
	go func() {
		for i := 0; i < 3; i++ {
			ev, _ := readFrame(t, br)
			got <- ev
		}
	}()
	deadline := time.After(5 * time.Second)
	for _, want := range []string{EventRunStarted, EventTextMessageStart, EventTextMessageContent} {
		select {
		case ev := <-got:
			if ev.Type != want {
				t.Fatalf("got %s, want %s", ev.Type, want)
			}
		case <-deadline:
			t.Fatalf("frame %s did not arrive while the runner was still blocked — response is buffered", want)
		}
	}
	close(release)
	rest := readAll(t, br)
	if len(rest) == 0 || rest[len(rest)-1].Type != EventRunFinished {
		t.Fatalf("tail: %+v", rest)
	}
}

func TestHandlerRunnerErrorBecomesRunError(t *testing.T) {
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "partial"})
		return errors.New("secret upstream detail")
	}
	srv := httptest.NewServer(NewHandler(run, Options{}))
	defer srv.Close()
	resp := post(t, srv.URL, runInput)
	defer resp.Body.Close()
	events := readAll(t, resp.Body)
	checkWellFormed(t, events)
	last := events[len(events)-1]
	if last.Type != EventRunError || last.Message != DefaultFriendlyError(errors.New("x")) || strings.Contains(last.Message, "secret") {
		t.Fatalf("last: %+v", last)
	}
	if events[len(events)-2].Type != EventTextMessageEnd {
		t.Fatalf("open message must be closed before RUN_ERROR: %+v", events)
	}
}

func TestHandlerRunnerPanicBecomesRunError(t *testing.T) {
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error { panic("kaboom") }
	srv := httptest.NewServer(NewHandler(run, Options{FriendlyError: func(err error) string { return "friendly: " + err.Error() }}))
	defer srv.Close()
	resp := post(t, srv.URL, runInput)
	defer resp.Body.Close()
	events := readAll(t, resp.Body)
	checkWellFormed(t, events)
	if last := events[len(events)-1]; last.Type != EventRunError || !strings.Contains(last.Message, "kaboom") {
		t.Fatalf("last: %+v", last)
	}
}

func TestHandlerMintsMissingIDs(t *testing.T) {
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error { return nil }
	srv := httptest.NewServer(NewHandler(run, Options{NewID: counter("id_")}))
	defer srv.Close()
	resp := post(t, srv.URL, `{"messages":[],"tools":[],"context":[]}`)
	defer resp.Body.Close()
	events := readAll(t, resp.Body)
	checkWellFormed(t, events)
	if events[0].ThreadID != "id_1" || events[0].RunID != "id_2" {
		t.Fatalf("ids: %+v", events[0])
	}
}

func TestHandlerRejects(t *testing.T) {
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error { return nil }
	srv := httptest.NewServer(NewHandler(run, Options{MaxBodyBytes: 64}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET: %d %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	resp = post(t, srv.URL, `{"threadId":`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d", resp.StatusCode)
	}
	resp = post(t, srv.URL, `{"threadId":"t","runId":"r","messages":[],"tools":[],"context":[],"forwardedProps":{"pad":"`+strings.Repeat("x", 80)+`"}}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", resp.StatusCode)
	}
}

func TestHandlerClientDisconnectCancelsRun(t *testing.T) {
	sawCancel := make(chan error, 1)
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error {
		emit(agent.StreamEvent{Type: agent.EventText, Delta: "…"})
		<-ctx.Done()
		sawCancel <- ctx.Err()
		return ctx.Err()
	}
	srv := httptest.NewServer(NewHandler(run, Options{}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(runInput))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if ev, _ := readFrame(t, br); ev.Type != EventRunStarted {
		t.Fatalf("first frame %+v", ev)
	}
	cancel()
	resp.Body.Close()
	select {
	case err := <-sawCancel:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runner saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner was not cancelled after the client went away")
	}
}

func TestHandlerRunTimeout(t *testing.T) {
	run := func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error {
		<-ctx.Done()
		return ctx.Err()
	}
	srv := httptest.NewServer(NewHandler(run, Options{RunTimeout: 20 * time.Millisecond}))
	defer srv.Close()
	resp := post(t, srv.URL, runInput)
	defer resp.Body.Close()
	events := readAll(t, resp.Body)
	checkWellFormed(t, events)
	if last := events[len(events)-1]; last.Type != EventRunError || !strings.Contains(last.Message, "too long") {
		t.Fatalf("last: %+v", last)
	}
}
