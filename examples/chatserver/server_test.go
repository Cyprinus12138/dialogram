package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/streambuf"
	"github.com/Cyprinus12138/dialogram/tool"
)

// upstream is a fake chat/completions endpoint: request n gets streams[n],
// and every request body is kept.
type upstream struct {
	mu      sync.Mutex
	streams []string
	bodies  []map[string]any
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	u.mu.Lock()
	n := len(u.bodies)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	if n >= len(u.streams) {
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, u.streams[n])
}

// messages returns the messages of upstream request i.
func (u *upstream) messages(t *testing.T, i int) []map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if i >= len(u.bodies) {
		t.Fatalf("upstream saw %d requests, want request %d", len(u.bodies), i+1)
	}
	var out []map[string]any
	for _, m := range u.bodies[i]["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func chunk(delta string) string {
	return fmt.Sprintf("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\n", delta)
}

func finish(reason string) string {
	return fmt.Sprintf("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", reason)
}

func callDelta(index int, id, name, args string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}`, index, id, name, a)
}

// testServer runs the example's routes over a real agent whose OpenAI
// upstream is u.
func testServer(t *testing.T, u *upstream) string {
	t.Helper()
	llm := httptest.NewServer(u)
	t.Cleanup(llm.Close)
	deps, checkpoints, err := buildDeps()
	if err != nil {
		t.Fatal(err)
	}
	ag, err := dialogram.New(dialogram.Settings{Provider: dialogram.ProviderOpenAI, OpenAIAPIKey: "test", OpenAIBaseURL: llm.URL, OpenAIModel: "m"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app{ag: ag, exec: deps.Executor, checkpoints: checkpoints, buf: streambuf.NewMemStore(0)}.routes())
	t.Cleanup(srv.Close)
	return srv.URL
}

type frame struct {
	event string
	data  map[string]any
}

// readStream reads SSE frames up to and including a terminal event.
func readStream(t *testing.T, resp *http.Response) []frame {
	t.Helper()
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var out []frame
	var cur frame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data)
		case line == "" && cur.event != "":
			out = append(out, cur)
			if cur.event == "done" || cur.event == "error" || cur.event == "waiting_for_user" {
				return out
			}
			cur = frame{}
		}
	}
	return out
}

func send(t *testing.T, base, msgID string, body any) []frame {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+"/chat/stream?message_id="+msgID, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return readStream(t, resp)
}

func tail(t *testing.T, base, msgID string) []frame {
	t.Helper()
	resp, err := http.Get(base + "/chat/stream?message_id=" + msgID + "&since=0")
	if err != nil {
		t.Fatal(err)
	}
	return readStream(t, resp)
}

func respond(t *testing.T, base string, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+"/chat/respond", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ended returns the terminal frame and the conversation id the stream
// announced.
func ended(t *testing.T, frames []frame) (frame, string) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("empty stream")
	}
	conv := ""
	for _, f := range frames {
		if f.event == "meta" {
			if m, _ := f.data["message"].(string); m != "" {
				conv = m
			}
		}
	}
	return frames[len(frames)-1], conv
}

func storedTurns(t *testing.T, base, conv string) []agent.Turn {
	t.Helper()
	resp, err := http.Get(base + "/conversations/" + conv)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c conversation
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c.Turns
}

func TestApprovalInAMultiToolTurnResumes(t *testing.T) {
	u := &upstream{streams: []string{
		chunk(`{"role":"assistant","content":"Searching, then booking."}`) +
			chunk(callDelta(0, "call_a", "search_flights", `{"from":"LIS","to":"MAD","date":"2026-11-02"}`)) +
			chunk(callDelta(1, "call_b", "book_flight", `{"carrier":"SKYLINE","departs":"09:10"}`)) +
			finish("tool_calls"),
		chunk(`{"role":"assistant","content":"Booked."}`) + finish("stop"),
	}}
	base := testServer(t, u)

	end, conv := ended(t, send(t, base, "m1", map[string]any{"message": "Book the 09:10 to Madrid."}))
	if end.event != "waiting_for_user" || end.data["kind"] != agent.PauseKindApproval || end.data["name"] != "book_flight" || conv == "" {
		t.Fatalf("the booking waits for approval: %s %v (conversation %q)", end.event, end.data, conv)
	}
	bookID := end.data["tool_use_id"].(string)

	resp := respond(t, base, map[string]any{"conversation_id": conv, "tool_use_id": bookID, "message_id": "m2", "approved": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("respond: %d", resp.StatusCode)
	}
	if end, _ := ended(t, tail(t, base, "m2")); end.event != "done" {
		t.Fatalf("the resumed turn completes: %s %v", end.event, end.data)
	}

	var searchResult, bookResult map[string]any
	for _, m := range u.messages(t, 1) {
		if m["role"] != "tool" {
			continue
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(m["content"].(string)), &body)
		if m["tool_call_id"] == bookID {
			bookResult = body
		} else {
			searchResult = body
		}
	}
	if searchResult["error_code"] != tool.ErrorCodeNotRun || bookResult["booked"] != true {
		t.Fatalf("both calls are answered on the wire: search %v, book %v", searchResult, bookResult)
	}
	if left := agent.DanglingToolUses(storedTurns(t, base, conv)); len(left) != 0 {
		t.Errorf("the stored conversation keeps no call without a result: %+v", left)
	}
	again := respond(t, base, map[string]any{"conversation_id": conv, "tool_use_id": bookID, "approved": true})
	again.Body.Close()
	if again.StatusCode != http.StatusConflict {
		t.Errorf("answering twice is refused, got %d", again.StatusCode)
	}
}

func TestNewMessageWhilePausedClosesThePause(t *testing.T) {
	choice := `{"prompt":"Which city?","options":[{"label":"Lisbon","value":"lis"},{"label":"Porto","value":"opo"}]}`
	u := &upstream{streams: []string{
		chunk(callDelta(0, "call_c", "request_user_choice", choice)) + finish("tool_calls"),
		chunk(`{"role":"assistant","content":"Porto it is."}`) + finish("stop"),
	}}
	base := testServer(t, u)

	end, conv := ended(t, send(t, base, "m1", map[string]any{"message": "Weather, please."}))
	if end.event != "waiting_for_user" || end.data["kind"] != agent.PauseKindHITLChoice {
		t.Fatalf("the model asks a question: %s %v", end.event, end.data)
	}
	choiceID := end.data["tool_use_id"].(string)

	if end, _ := ended(t, send(t, base, "m2", map[string]any{"conversation_id": conv, "message": "Porto, actually."})); end.event != "done" {
		t.Fatalf("the new message runs: %s %v", end.event, end.data)
	}
	msgs := u.messages(t, 1)
	at := -1
	for i, m := range msgs {
		if m["role"] == "tool" && m["tool_call_id"] == choiceID {
			at = i
		}
	}
	if at < 0 || !strings.Contains(msgs[at]["content"].(string), tool.ErrorCodeCancelled) {
		t.Fatalf("the unanswered question is closed as cancelled: %v", msgs)
	}
	if at+1 >= len(msgs) || msgs[at+1]["role"] != "user" || msgs[at+1]["content"] != "Porto, actually." {
		t.Errorf("the new message follows the closed question: %v", msgs)
	}
}
