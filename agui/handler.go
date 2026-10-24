package agui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Runner runs one AG-UI run: typically convert in.Messages with
// MessagesToTurns and call agent.Agent.ChatStream with emit. It runs on the
// request goroutine; ctx is cancelled when the client disconnects or the
// run timeout elapses, and the Runner should return then. A returned error
// becomes RUN_ERROR unless the run already ended.
type Runner func(ctx context.Context, in RunAgentInput, emit agent.EmitFn) error

// Options tune the handler. Every field is optional.
type Options struct {
	// MaxBodyBytes caps the request body. Default 1 MiB.
	MaxBodyBytes int64
	// RunTimeout bounds one run. Default 5 minutes.
	RunTimeout time.Duration
	// FriendlyError maps a Runner error to the RUN_ERROR message clients
	// see; raw provider errors should not reach end users. Default
	// DefaultFriendlyError. sse.DefaultFriendlyError is a richer option.
	FriendlyError func(error) string
	// NewID mints message ids and fills in a blank threadId / runId. It is
	// called from concurrent requests, so it must be safe for concurrent
	// use. Default NewID.
	NewID func() string
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// Executor runs a tool call the user approved when a run resumes from
	// an approval interrupt. Pass the executor the agent uses
	// (dialogram.Deps.Executor, or tool.NewLocalExecutor over the same
	// registry). Nil means approvals can be declined or cancelled but an
	// approving resume is refused with RUN_ERROR.
	Executor tool.Executor
	// InterruptKey signs interrupt ids, so a resume can only answer a tool
	// call this endpoint paused on, in the same thread, with the arguments
	// the user saw. Every instance serving one endpoint must share it.
	// Empty means a random key per Handler: interrupts then do not survive
	// a restart and cannot be resumed on another instance.
	InterruptKey []byte
	// InterruptTTL is how long an interrupt can be answered; it is sent as
	// expiresAt. A resolved resume after it is refused, a cancelled one is
	// still accepted so the thread can move on. The result of an approved
	// call is kept for as long, so a replayed resume does not run the tool
	// again. Zero means DefaultInterruptTTL; negative means interrupts never
	// expire, and a replay older than DefaultInterruptTTL can then run an
	// approved call a second time.
	InterruptTTL time.Duration
}

// DefaultInterruptTTL is how long an interrupt stays answerable when
// Options.InterruptTTL is zero.
const DefaultInterruptTTL = 24 * time.Hour

func (o *Options) defaults() {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1 << 20
	}
	if o.RunTimeout <= 0 {
		o.RunTimeout = 5 * time.Minute
	}
	if o.FriendlyError == nil {
		o.FriendlyError = DefaultFriendlyError
	}
	if o.NewID == nil {
		o.NewID = NewID
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.InterruptTTL == 0 {
		o.InterruptTTL = DefaultInterruptTTL
	}
}

// DefaultFriendlyError maps a timeout to one line and everything else to a
// generic one. The full error is logged server-side.
func DefaultFriendlyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "The model took too long to respond. Please try again."
	}
	return "The AI service is temporarily unavailable. Please try again in a moment."
}

// Handler is the AG-UI endpoint: POST a RunAgentInput, receive the run as
// text/event-stream frames. One run per request, and no server-side buffer:
// AG-UI defines no cursor to resume a dropped stream from (package sse
// does that). Runs that end on an interrupt are resumed the AG-UI way, by a
// new run whose input carries resume entries; see docs/AGUI.md.
type Handler struct {
	run      Runner
	opts     Options
	codec    *interruptCodec
	approved *resultCache
}

// NewHandler builds a Handler over run.
func NewHandler(run Runner, opts Options) *Handler {
	if run == nil {
		panic("agui: NewHandler needs a Runner")
	}
	opts.defaults()
	key := opts.InterruptKey
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	keep := opts.InterruptTTL
	if keep < 0 {
		keep = DefaultInterruptTTL
	}
	return &Handler{
		run:      run,
		opts:     opts,
		codec:    &interruptCodec{key: key, ttl: opts.InterruptTTL, now: time.Now},
		approved: newResultCache(keep, time.Now),
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.opts.MaxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > h.opts.MaxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var in RunAgentInput
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "invalid RunAgentInput: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The messages once more, as sent, for the snapshot an interrupt
	// requires: it must not rewrite content Message cannot hold.
	var sent struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(body, &sent)
	if in.ThreadID == "" {
		in.ThreadID = h.opts.NewID()
	}
	if in.RunID == "" {
		in.RunID = h.opts.NewID()
	}

	hdr := w.Header()
	hdr.Set("Content-Type", ContentType)
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// The encoder and the writer are shared with the Runner's emit calls.
	// Those are serial by contract, but a Runner that leaks a goroutine
	// must neither race the encoder nor write to a finished response, so
	// everything goes through one mutex and emit turns inert once the
	// handler is done.
	threadID := in.ThreadID
	enc := NewEncoder(EncoderOptions{ThreadID: in.ThreadID, RunID: in.RunID, NewID: h.opts.NewID, History: sent.Messages,
		InterruptID: func(p Pause) (string, time.Time) { return h.codec.mint(threadID, p) }})
	var mu sync.Mutex
	closed := false
	send := func(events []Event) { // caller holds mu
		if closed || len(events) == 0 {
			return
		}
		for _, ev := range events {
			if err := WriteFrame(w, ev); err != nil {
				return
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit := func(ev agent.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		send(enc.Push(ev))
	}
	finish := func(events func() []Event) {
		mu.Lock()
		defer mu.Unlock()
		send(events())
		closed = true
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.opts.RunTimeout)
	defer cancel()
	log := h.opts.Logger.With("thread_id", in.ThreadID, "run_id", in.RunID)

	// RUN_STARTED goes out before the Runner does anything, so the client
	// sees the run open even if the first model byte is slow.
	emit(agent.StreamEvent{Type: agent.EventMeta, MessageID: in.RunID})
	started := time.Now()

	// A run that continues from a pause starts with the paused calls'
	// results, or is refused before the Runner sees it.
	plan, err := h.planResume(ctx, in, sent.Messages)
	if err != nil {
		var refusal *resumeError
		msg, code := h.opts.FriendlyError(err), ""
		if errors.As(err, &refusal) {
			msg, code = refusal.msg, refusal.code
		}
		log.Warn("agui resume refused", "code", code, "err", err)
		finish(func() []Event { return enc.fail(msg, code) })
		return
	}
	if plan != nil && plan.again != nil {
		// The resumed call's tool asked for more input: the run ends on a
		// new interrupt without the model.
		a := plan.again
		mu.Lock()
		enc.history = plan.raw
		send(enc.askAgain(a.call.id, a.call.name, a.call.args, a.req))
		mu.Unlock()
		finish(enc.Finish)
		log.Info("agui resume asks again", "tool", a.call.name, "tool_call_id", a.call.id)
		return
	}
	if plan != nil {
		mu.Lock()
		enc.history = plan.raw
		send(enc.answered(plan.results))
		mu.Unlock()
		in.Messages = plan.messages
	}

	err = h.safeRun(ctx, in, emit)
	switch {
	case err == nil:
		finish(enc.Finish)
		log.Info("agui run done", "duration_ms", time.Since(started).Milliseconds())
	case r.Context().Err() != nil:
		// The client went away; nothing can be written to it.
		finish(func() []Event { return nil })
		log.Info("agui run cancelled by client", "duration_ms", time.Since(started).Milliseconds())
	default:
		log.Error("agui run failed", "err", err, "duration_ms", time.Since(started).Milliseconds())
		finish(func() []Event {
			return enc.Push(agent.StreamEvent{Type: agent.EventError, Message: h.opts.FriendlyError(err)})
		})
	}
}

// safeRun turns a panicking Runner into an error so the client still gets
// a terminal event.
func (h *Handler) safeRun(ctx context.Context, in RunAgentInput, emit agent.EmitFn) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			h.opts.Logger.Error("agui runner panic", "panic", rec, "thread_id", in.ThreadID, "run_id", in.RunID)
			err = fmt.Errorf("agui: runner panic: %v", rec)
		}
	}()
	return h.run(ctx, in, emit)
}
