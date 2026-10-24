package agui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// RUN_ERROR codes for a resume the Handler refuses, one per class of the
// spec's contract violations.
const (
	// CodeInterruptPending: the conversation waits on an interrupt and the
	// input does not resume it.
	CodeInterruptPending = "INTERRUPT_PENDING"
	// CodeResumeInvalid: a resume entry is malformed, names an interrupt
	// this handler did not raise, or carries a payload that does not fit.
	CodeResumeInvalid = "RESUME_INVALID"
	// CodeResumeExpired: a resolved resume arrived after expiresAt.
	CodeResumeExpired = "RESUME_EXPIRED"
	// CodeHistoryInvalid: an earlier assistant message has a tool call no
	// tool message answers.
	CodeHistoryInvalid = "HISTORY_INVALID"
)

// resumeError is a refusal the client can act on; its text goes out on
// RUN_ERROR as written.
type resumeError struct {
	code string
	msg  string
}

func (e *resumeError) Error() string { return e.msg }

func refuse(code, format string, args ...any) error {
	return &resumeError{code: code, msg: fmt.Sprintf(format, args...)}
}

// interruptCodec mints and checks interrupt ids. An id names the paused
// tool call and carries an HMAC over the thread, the call's id, name and
// canonical arguments, the pause kind and the expiry, so a resume can only
// answer a call this handler paused on, in the same thread, with the
// arguments the user was shown:
//
//	dgi1.<base64url tool call id>.<pause kind>.<expiry in unix seconds, 0 for none>.<base64url HMAC-SHA256, 16 bytes>
//
// The id is opaque to clients, who echo it back in resume[].interruptId.
type interruptCodec struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

const interruptIDVersion = "dgi1"

var b64 = base64.RawURLEncoding

// interruptRef is a parsed interrupt id, not yet verified.
type interruptRef struct {
	toolCallID string
	kind       string
	expiry     string
	mac        []byte
}

func (c *interruptCodec) mint(threadID string, p Pause) (string, time.Time) {
	var expires time.Time
	expiry := "0"
	if c.ttl > 0 {
		expires = c.now().Add(c.ttl).Truncate(time.Second)
		expiry = strconv.FormatInt(expires.Unix(), 10)
	}
	mac := c.sign(threadID, p.ToolUseID, p.Kind, expiry, p.Name, p.Input)
	return strings.Join([]string{interruptIDVersion, b64.EncodeToString([]byte(p.ToolUseID)), p.Kind, expiry, b64.EncodeToString(mac)}, "."), expires
}

func (c *interruptCodec) parse(id string) (interruptRef, bool) {
	parts := strings.Split(id, ".")
	if len(parts) != 5 || parts[0] != interruptIDVersion {
		return interruptRef{}, false
	}
	callID, err := b64.DecodeString(parts[1])
	if err != nil || len(callID) == 0 {
		return interruptRef{}, false
	}
	if _, err := strconv.ParseInt(parts[3], 10, 64); err != nil {
		return interruptRef{}, false
	}
	mac, err := b64.DecodeString(parts[4])
	if err != nil {
		return interruptRef{}, false
	}
	switch parts[2] {
	case agent.PauseKindHITL, agent.PauseKindHITLChoice, agent.PauseKindHITLForm, agent.PauseKindApproval, agent.PauseKindCustom:
	default:
		return interruptRef{}, false
	}
	return interruptRef{toolCallID: string(callID), kind: parts[2], expiry: parts[3], mac: mac}, true
}

func (c *interruptCodec) verify(threadID string, ref interruptRef, name string, args json.RawMessage) bool {
	return hmac.Equal(c.sign(threadID, ref.toolCallID, ref.kind, ref.expiry, name, args), ref.mac)
}

// expired reports whether ref's expiry has passed.
func (c *interruptCodec) expired(ref interruptRef) bool {
	n, _ := strconv.ParseInt(ref.expiry, 10, 64)
	return n > 0 && !c.now().Before(time.Unix(n, 0))
}

func (c *interruptCodec) sign(threadID, callID, kind, expiry, name string, args json.RawMessage) []byte {
	m := hmac.New(sha256.New, c.key)
	for _, part := range []string{interruptIDVersion, threadID, callID, kind, expiry, name, string(canonicalJSON(args))} {
		fmt.Fprintf(m, "%d:%s;", len(part), part)
	}
	return m.Sum(nil)[:16]
}

// canonicalJSON renders a JSON value with sorted keys, no insignificant
// whitespace and numbers as written, so the arguments a client echoes back
// verify whatever formatting it applied. Empty means {}, the way the
// Encoder announces a call without arguments.
func canonicalJSON(raw json.RawMessage) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// historyCall is one tool call of an assistant message in the history.
type historyCall struct {
	msgIndex int
	id       string
	name     string
	args     json.RawMessage
}

// resumeResult is one tool message the Handler adds at the start of a
// resumed run, with what the Encoder needs to announce it.
type resumeResult struct {
	msg      Message
	name     string
	args     json.RawMessage
	executed bool // the result is a tool's real output, so it may render
}

// resumePlan is how a run starts when it continues from a pause: the
// history with the new tool messages in place, for the Runner (structs)
// and for the interrupt snapshot (raw, so the client's own messages are
// echoed byte for byte), and the results to announce.
type resumePlan struct {
	messages []Message
	raw      []json.RawMessage
	results  []resumeResult
}

// decision is one resume entry checked against the history.
type decision struct {
	entry    ResumeEntry
	call     historyCall
	ref      interruptRef
	approval approvalAnswer
}

// planResume applies the spec's interrupt contract to an incoming run. It
// returns nil when the run starts as an ordinary run, a plan when tool
// messages must be added first, and a *resumeError when the input breaks
// the contract. Every entry is checked before any approved tool runs, so a
// refusal never leaves half a resume applied.
//
// The handler keeps no conversation state: the client's history is the
// record, and the signed interrupt id is what makes it trustworthy enough
// to act on. Only the tool calls of the last assistant message that has
// any can be pending; one of them is the interrupted call, and the rest of
// that message's unanswered calls were never run (the loop surfaces one
// pause at a time and runs nothing in a turn it pauses for approval), so
// they are answered with ErrorCodeNotRun.
func (h *Handler) planResume(ctx context.Context, in RunAgentInput, raw []json.RawMessage) (*resumePlan, error) {
	answered := map[string]bool{}
	for _, m := range in.Messages {
		if m.Role == RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	last := -1
	for i, m := range in.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			last = i
		}
	}
	calls := map[string]historyCall{}
	var pending []historyCall
	for i, m := range in.Messages {
		if m.Role != RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			hc := historyCall{msgIndex: i, id: tc.ID, name: tc.Function.Name, args: json.RawMessage(tc.Function.Arguments)}
			calls[tc.ID] = hc
			if answered[tc.ID] {
				continue
			}
			if i != last {
				return nil, refuse(CodeHistoryInvalid, "messages[%d]: tool call %s (%s) has no result", i, tc.ID, tc.Function.Name)
			}
			pending = append(pending, hc)
		}
	}

	if len(in.Resume) == 0 {
		if len(pending) == 0 {
			return nil, nil
		}
		// A client that answers a pause the pre-interrupt way, with a tool
		// message of its own, leaves the paused call's siblings unanswered.
		for _, tc := range in.Messages[last].ToolCalls {
			if answered[tc.ID] {
				return h.complete(in, raw, last, pending, nil), nil
			}
		}
		return nil, refuse(CodeInterruptPending, "the conversation is waiting on an interrupt for tool call %s (%s); resume it with status %q or %q before sending new input",
			pending[0].id, pending[0].name, ResumeResolved, ResumeCancelled)
	}

	var todo []decision
	seen := map[string]bool{}
	for i, e := range in.Resume {
		at := fmt.Sprintf("resume[%d]", i)
		if seen[e.InterruptID] {
			return nil, refuse(CodeResumeInvalid, "%s: interrupt %q is answered twice", at, e.InterruptID)
		}
		seen[e.InterruptID] = true
		if e.Status != ResumeResolved && e.Status != ResumeCancelled {
			return nil, refuse(CodeResumeInvalid, "%s: status must be %q or %q, not %q", at, ResumeResolved, ResumeCancelled, e.Status)
		}
		ref, ok := h.codec.parse(e.InterruptID)
		if !ok {
			return nil, refuse(CodeResumeInvalid, "%s: %q is not an interrupt this agent raised", at, e.InterruptID)
		}
		call, ok := calls[ref.toolCallID]
		if !ok || !h.codec.verify(in.ThreadID, ref, call.name, call.args) {
			return nil, refuse(CodeResumeInvalid, "%s: interrupt %q does not match a tool call in this conversation's history", at, e.InterruptID)
		}
		if answered[call.id] {
			// Already applied: the history holds the result, so this is a
			// replay and there is nothing left to do for it.
			continue
		}
		d := decision{entry: e, call: call, ref: ref}
		if e.Status == ResumeResolved {
			if h.codec.expired(ref) {
				return nil, refuse(CodeResumeExpired, "%s: interrupt %q has expired; resume it with status %q to move on", at, e.InterruptID, ResumeCancelled)
			}
			p := bytes.TrimSpace(e.Payload)
			if len(p) == 0 || bytes.Equal(p, []byte("null")) {
				return nil, refuse(CodeResumeInvalid, "%s: a resolved resume needs a payload", at)
			}
			if !json.Valid(p) {
				return nil, refuse(CodeResumeInvalid, "%s: the payload is not valid JSON", at)
			}
			a, err := checkPayload(ref.kind, p, call.args)
			if err != nil {
				return nil, refuse(CodeResumeInvalid, "%s: %v", at, err)
			}
			d.approval = a
			if ref.kind == agent.PauseKindApproval && a.Approved && h.opts.Executor == nil {
				return nil, refuse(CodeResumeInvalid, "%s: this endpoint cannot run approved tool calls (no executor is configured)", at)
			}
		}
		todo = append(todo, d)
	}
	if len(todo) == 0 {
		if len(pending) > 0 {
			return nil, refuse(CodeInterruptPending, "the resume does not answer the open interrupt for tool call %s (%s)", pending[0].id, pending[0].name)
		}
		return nil, nil
	}

	answers := map[string]resumeResult{}
	for _, d := range todo {
		answers[d.call.id] = h.resolve(ctx, in.ThreadID, d)
	}
	return h.complete(in, raw, last, pending, answers), nil
}

// resolve turns one checked decision into the paused call's result. Only
// an approved call runs anything; the executor's output is cached under
// the interrupt id, so a replayed resume that reaches this point again
// (the client never saw the first result) reuses it instead of running
// the tool twice.
func (h *Handler) resolve(ctx context.Context, threadID string, d decision) resumeResult {
	r := resumeResult{name: d.call.name, args: d.call.args}
	switch {
	case d.entry.Status == ResumeCancelled:
		r.msg.Content = agent.DismissedResultJSON(d.ref.kind, "")
	case d.ref.kind == agent.PauseKindApproval && !d.approval.Approved:
		reason := strings.TrimSpace(d.approval.Reason)
		if reason == "" {
			reason = "The user declined to run this tool."
		}
		r.msg.Content = agent.DeniedResultJSON(reason)
	case d.ref.kind == agent.PauseKindApproval:
		r.executed = true
		r.msg.Content = clientView(h.approved.do(threadID+"\x00"+d.entry.InterruptID, func() string {
			args := json.RawMessage(bytes.TrimSpace(d.call.args))
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			// The user approved this call, so it runs to completion even if
			// the client goes away meanwhile; a retried resume then finds
			// the result in the cache instead of running the tool again.
			execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.opts.RunTimeout)
			defer cancel()
			h.opts.Logger.Info("agui resume runs an approved tool", "tool", d.call.name, "tool_call_id", d.call.id)
			return h.execute(execCtx, tool.Call{ToolUseID: d.call.id, Name: d.call.name, Input: args})
		}))
	default:
		var buf bytes.Buffer
		_ = json.Compact(&buf, d.entry.Payload)
		r.msg.Content = buf.String()
	}
	return r
}

// execute runs an approved call. A failing or panicking executor becomes a
// hard-failure result for the model rather than a broken response.
func (h *Handler) execute(ctx context.Context, call tool.Call) (out string) {
	defer func() {
		if rec := recover(); rec != nil {
			h.opts.Logger.Error("agui executor panic", "panic", rec, "tool", call.Name)
			out = softJSON(tool.ErrorCodeHardFailure, fmt.Sprintf("the tool failed: %v", rec))
		}
	}()
	out, err := h.opts.Executor.Execute(ctx, call)
	if err != nil {
		return softJSON(tool.ErrorCodeHardFailure, err.Error())
	}
	return out
}

// complete answers every pending call of the last assistant message, with
// answers where the resume gave one and ErrorCodeNotRun elsewhere, and
// places the new tool messages right after that message's existing tool
// messages, where providers require tool results to sit.
func (h *Handler) complete(in RunAgentInput, raw []json.RawMessage, last int, pending []historyCall, answers map[string]resumeResult) *resumePlan {
	plan := &resumePlan{}
	for _, c := range pending {
		r, ok := answers[c.id]
		if !ok {
			r = resumeResult{name: c.name, args: c.args}
			r.msg.Content = agent.NotRunResultJSON(c.name)
		}
		r.msg.ID, r.msg.Role, r.msg.ToolCallID = h.opts.NewID(), RoleTool, c.id
		plan.results = append(plan.results, r)
	}
	at := last + 1
	for at < len(in.Messages) && in.Messages[at].Role == RoleTool {
		at++
	}
	for i := range in.Messages {
		if i == at {
			plan.appendResults()
		}
		plan.messages = append(plan.messages, in.Messages[i])
		if i < len(raw) {
			plan.raw = append(plan.raw, raw[i])
		} else {
			plan.raw = append(plan.raw, mustJSON(in.Messages[i]))
		}
	}
	if at >= len(in.Messages) {
		plan.appendResults()
	}
	return plan
}

func (p *resumePlan) appendResults() {
	for _, r := range p.results {
		p.messages = append(p.messages, r.msg)
		p.raw = append(p.raw, mustJSON(r.msg))
	}
}

// resultCache remembers the output of approved tool calls for a while, and
// lets concurrent identical resumes share one execution.
type resultCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	done    map[string]cachedResult
	running map[string]*flight
}

type cachedResult struct {
	out string
	at  time.Time
}

type flight struct {
	wait chan struct{}
	out  string
}

// approvedCacheSize bounds the approved results a Handler remembers.
const approvedCacheSize = 4096

func newResultCache(ttl time.Duration, now func() time.Time) *resultCache {
	return &resultCache{ttl: ttl, max: approvedCacheSize, now: now, done: map[string]cachedResult{}, running: map[string]*flight{}}
}

func (c *resultCache) do(key string, run func() string) string {
	c.mu.Lock()
	if r, ok := c.done[key]; ok && c.now().Sub(r.at) < c.ttl {
		c.mu.Unlock()
		return r.out
	}
	if f, ok := c.running[key]; ok {
		c.mu.Unlock()
		<-f.wait
		return f.out
	}
	f := &flight{wait: make(chan struct{})}
	c.running[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.running, key)
		c.store(key, f.out)
		c.mu.Unlock()
		close(f.wait)
	}()
	f.out = run()
	return f.out
}

// store records out under key, dropping expired entries and then the
// oldest ones beyond the size cap. Caller holds mu.
func (c *resultCache) store(key, out string) {
	now := c.now()
	c.done[key] = cachedResult{out: out, at: now}
	if len(c.done) <= c.max {
		return
	}
	for k, r := range c.done {
		if now.Sub(r.at) >= c.ttl {
			delete(c.done, k)
		}
	}
	for len(c.done) > c.max {
		oldest, at := "", now
		for k, r := range c.done {
			if !r.at.After(at) {
				oldest, at = k, r.at
			}
		}
		delete(c.done, oldest)
	}
}

// clientView is tool.ClientView for a result held as a string.
func clientView(out string) string { return string(tool.ClientView(json.RawMessage(out))) }
