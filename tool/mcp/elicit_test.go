package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/dialogram/tool"
)

// elicitServer has tools that ask the user before they answer, the
// 2026-07-28 way: an input-required result, then a retry with the answer.
func elicitServer(t *testing.T) *Client {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "asker", Version: "0"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{"seat": map[string]any{"type": "string"}}, "required": []any{"seat"}}
	// book asks for a seat, then for a confirmation, then books.
	sdk.AddTool(srv, &sdk.Tool{Name: "book", Description: "Books a seat."},
		func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			text := func(s string) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}, nil, nil
			}
			switch req.Params.RequestState {
			case "":
				return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"seat": &sdk.ElicitParams{Message: "Which seat?", RequestedSchema: schema}}, RequestState: "asked-seat"}, nil, nil
			case "asked-seat":
				r, _ := req.Params.InputResponses["seat"].(*sdk.ElicitResult)
				if r == nil {
					return text("no answer came back")
				}
				if r.Action != tool.InputAccept {
					return text("not booked: " + r.Action)
				}
				return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"pay": &sdk.ElicitParams{Mode: "url", Message: "Pay for seat " + r.Content["seat"].(string), URL: "https://pay.example/1"}}, RequestState: "seat=" + r.Content["seat"].(string)}, nil, nil
			default:
				if r, _ := req.Params.InputResponses["pay"].(*sdk.ElicitResult); r == nil || r.Action != tool.InputAccept {
					return text("not paid")
				}
				return text("booked " + strings.TrimPrefix(req.Params.RequestState, "seat="))
			}
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "think", Description: "Wants a model."},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"m": &sdk.CreateMessageParams{MaxTokens: 10}}, RequestState: "x"}, nil, nil
		})
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Connect(context.Background(), rawTransport{ct}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(); _ = ss.Close() })
	return c
}

func TestElicitationBecomesToolInput(t *testing.T) {
	c := elicitServer(t)
	reg := tool.NewRegistry()
	if _, err := Register(context.Background(), reg, c, RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	exec := tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{})
	call := tool.Call{ToolUseID: "dg_1", Name: "asker__book", Input: json.RawMessage(`{}`)}

	_, err := exec.Execute(context.Background(), call)
	req, ok := tool.AsInputRequired(err)
	if !ok || req.Mode != tool.InputModeForm || req.Message != "Which seat?" || !strings.Contains(string(req.Schema), `"seat"`) {
		t.Fatalf("first run: err=%v req=%+v", err, req)
	}
	out, again, err := tool.ExecuteWithInput(context.Background(), exec, call, *req, tool.InputResponse{Action: tool.InputAccept, Content: map[string]any{"seat": "12A"}})
	if err != nil || out != "" || again == nil || again.Mode != tool.InputModeURL || again.URL != "https://pay.example/1" || again.Message != "Pay for seat 12A" {
		t.Fatalf("second run asks again: out=%s again=%+v err=%v", out, again, err)
	}
	out, again, err = tool.ExecuteWithInput(context.Background(), exec, call, *again, tool.InputResponse{Action: tool.InputAccept})
	if err != nil || again != nil || !strings.Contains(out, "booked 12A") {
		t.Fatalf("third run books: out=%s again=%+v err=%v", out, again, err)
	}

	out, again, err = tool.ExecuteWithInput(context.Background(), exec, call, *req, tool.InputResponse{Action: tool.InputDecline})
	if err != nil || again != nil || !strings.Contains(out, "not booked: decline") {
		t.Fatalf("a declined answer reaches the server: out=%s err=%v", out, err)
	}
}

func TestUnsupportedInputIsASoftFailure(t *testing.T) {
	c := elicitServer(t)
	reg := tool.NewRegistry()
	if _, err := Register(context.Background(), reg, c, RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := reg.Call(context.Background(), "asker__think", `{}`)
	if err != nil || !strings.Contains(out, ErrorCodeInputUnsupported) {
		t.Fatalf("a sampling request is reported to the model: out=%s err=%v", out, err)
	}
	if _, _, err := tool.ExecuteWithInput(context.Background(), tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{}),
		tool.Call{Name: "asker__book", Input: json.RawMessage(`{}`)}, tool.InputRequest{State: json.RawMessage(`"not ours"`)}, tool.InputResponse{Action: tool.InputAccept}); err == nil {
		t.Error("a request state this client did not issue is refused")
	}
}

// A server that negotiated an older protocol (stateful streamable HTTP
// falls back to 2025-11-25) sends elicitation as a request in the middle of
// the call, which a paused run cannot answer: the call fails, and says so.
func TestLegacyElicitationFails(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "old", Version: "0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "ask"}, func(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		_, err := req.Session.Elicit(ctx, &sdk.ElicitParams{Message: "Sure?"})
		return nil, nil, err
	})
	hs := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	defer hs.Close()
	c, err := Connect(context.Background(), StreamableHTTP{URL: hs.URL}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reg := tool.NewRegistry()
	if _, err := Register(context.Background(), reg, c, RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := reg.Call(context.Background(), "old__ask", `{}`)
	if _, asked := tool.AsInputRequired(err); asked || !strings.Contains(out+fmt.Sprint(err), "elicitation") {
		t.Errorf("out=%s err=%v", out, err)
	}
}
