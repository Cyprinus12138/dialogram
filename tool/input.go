package tool

import (
	"context"
	"encoding/json"
	"errors"
)

// A tool that cannot finish a call without the user's input returns
// NeedInput(req) from its Fn. The loop then pauses the run with a
// waiting_for_user event of kind tool_input carrying req, and leaves the
// call without a result. The application asks the user and runs the call
// again with ExecuteWithInput, which hands the tool the request it raised
// and the user's answer through the context (InputResponseFrom); the
// result answers the paused call (agent.AnswerPause). A tool may ask again
// on the second run, as many rounds as it needs.
//
// This is how an MCP server's elicitation reaches the user (tool/mcp), and
// any local tool can use it the same way.

// Input request modes.
const (
	// InputModeForm asks the user to fill in fields described by
	// InputRequest.Schema.
	InputModeForm = "form"
	// InputModeURL asks the user to complete a step at InputRequest.URL (a
	// sign-in, a payment page) and confirm.
	InputModeURL = "url"
)

// Actions a user takes on an input request.
const (
	InputAccept  = "accept"  // answered: Content holds the fields (form) or nothing (url)
	InputDecline = "decline" // said no
	InputCancel  = "cancel"  // dismissed without deciding
)

// InputRequest is what a tool needs from the user before it can finish a
// call.
type InputRequest struct {
	Mode    string `json:"mode"`
	Message string `json:"message"`
	// Schema describes the answer for InputModeForm: a JSON Schema object
	// with flat properties of type string, number, integer or boolean
	// (strings may carry an enum).
	Schema json.RawMessage `json:"requested_schema,omitempty"`
	// URL is the page to open for InputModeURL.
	URL string `json:"url,omitempty"`
	// State is opaque to everything but the tool that raised the request;
	// the application keeps it with the pause and hands it back unchanged.
	State json.RawMessage `json:"state,omitempty"`
}

// InputResponse is the user's answer to an InputRequest.
type InputResponse struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// InputRequiredError is the error NeedInput returns. The loop recognises it
// with errors.As anywhere in an executor's error chain.
type InputRequiredError struct {
	Request InputRequest
}

func (e *InputRequiredError) Error() string {
	return "tool: the call needs the user's input: " + e.Request.Message
}

// NeedInput is what a tool's Fn returns to pause the call on req.
func NeedInput(req InputRequest) error {
	if req.Mode == "" {
		req.Mode = InputModeForm
	}
	return &InputRequiredError{Request: req}
}

// AsInputRequired reports whether err asks for the user's input.
func AsInputRequired(err error) (*InputRequest, bool) {
	var e *InputRequiredError
	if errors.As(err, &e) {
		return &e.Request, true
	}
	return nil, false
}

type inputCtxKey struct{}

type inputAnswer struct {
	req  InputRequest
	resp InputResponse
}

// WithInputResponse returns ctx carrying the answer resp to req, for the
// tool's second run.
func WithInputResponse(ctx context.Context, req InputRequest, resp InputResponse) context.Context {
	return context.WithValue(ctx, inputCtxKey{}, inputAnswer{req: req, resp: resp})
}

// InputResponseFrom returns the request a tool raised on an earlier run of
// this call and the user's answer to it, when ctx carries one.
func InputResponseFrom(ctx context.Context) (InputRequest, InputResponse, bool) {
	a, ok := ctx.Value(inputCtxKey{}).(inputAnswer)
	return a.req, a.resp, ok
}

// ExecuteWithInput runs call again through exec with the user's answer to
// req. When the tool asks once more, again holds the new request and out is
// empty; the application pauses on it like on the first.
func ExecuteWithInput(ctx context.Context, exec Executor, call Call, req InputRequest, resp InputResponse) (out string, again *InputRequest, err error) {
	out, err = exec.Execute(WithInputResponse(ctx, req, resp), call)
	if r, ok := AsInputRequired(err); ok {
		return "", r, nil
	}
	return out, nil, err
}
