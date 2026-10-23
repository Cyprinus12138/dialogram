package agui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Cyprinus12138/dialogram/agent"
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
	// NewID mints message ids and fills in a blank threadId / runId.
	// Default NewID.
	NewID func() string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

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
// text/event-stream frames. One run per request; there is no server-side
// buffer and no resume (AG-UI defines no cursor) — see package sse for
// that.
type Handler struct {
	run  Runner
	opts Options
}

// NewHandler builds a Handler over run.
func NewHandler(run Runner, opts Options) *Handler {
	if run == nil {
		panic("agui: NewHandler needs a Runner")
	}
	opts.defaults()
	return &Handler{run: run, opts: opts}
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
	enc := NewEncoder(EncoderOptions{ThreadID: in.ThreadID, RunID: in.RunID, NewID: h.opts.NewID})
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
