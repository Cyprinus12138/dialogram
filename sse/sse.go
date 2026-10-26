// Package sse serves a resumable chat stream over Server-Sent Events.
//
// The producer (the model call) is a first-class server-side resource; the
// HTTP connection is disposable. Each turn has a stable, client-chosen
// message id; the producer writes every event to a streambuf.Store keyed by
// that id, and any number of connections can tail the buffer from any
// offset. A client that loses its connection reconnects with the last
// `id:` it saw and receives exactly the events it missed.
//
// Protocol:
//
//	POST /chat/stream?message_id=<id>&since=<seq>   body: application-defined
//	GET  /chat/stream?message_id=<id>&since=<seq>   pure tail (EventSource-friendly)
//	POST /chat/stream/cancel?message_id=<id>
//
// Response: text/event-stream, one frame per event with `id:` set to the
// event's sequence number and `event:` mirroring the JSON "type" field.
// `: ping` comment lines keep proxies happy and carry no id. The stream
// ends on a `done`, `error` or `waiting_for_user` event; the buffer's
// terminal status is set at the same time.
//
// Tracing: the producer's detached context carries the span context of
// the request that launched it, so the run's spans are children of that
// request's span in one trace. Every request's span gets the
// dialogram.message_id attribute, and a request that tails a producer
// running in this process also gets a link to the launching request's
// span. See docs/OBSERVABILITY.md.
package sse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/internal/telemetry"
	"github.com/Cyprinus12138/dialogram/streambuf"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Producer runs one turn. body is the raw request body the first connect
// carried (the application decides its shape: a conversation id, a message,
// a full history); emit receives every stream event. It runs in a detached
// context that outlives the HTTP request and should return when ctx is
// done.
type Producer func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error

// Usage is the final per-turn token usage the producer reported.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// Result is what a finished producer left behind, handed to
// Options.OnFinish so the application can persist turns, pending pauses and
// usage regardless of how the turn ended.
type Result struct {
	MessageID string
	// Turns are the assistant and tool turns produced, in order.
	Turns []agent.Turn
	// Status is the terminal buffer status: streambuf.StatusDone,
	// streambuf.StatusWaiting, or an error status.
	Status string
	// Paused is set when the producer ended on a waiting_for_user event.
	Paused          bool
	PausedKind      string
	PausedToolUseID string
	PausedToolName  string
	PausedToolInput json.RawMessage
	PausedPrompt    string
	// PausedRequest is the tool.InputRequest of a tool_input pause.
	PausedRequest json.RawMessage
	Usage         Usage
	// Err is the producer's error, if any (nil on cancel).
	Err error
	// Cancelled is true when the client stopped the stream.
	Cancelled bool
	// EmittedText reports whether any text reached the buffer — useful to
	// decide whether a failed turn is worth persisting.
	EmittedText bool
}

// Options tune the handler. Every field is optional.
type Options struct {
	// ProducerTimeout bounds one producer run. Default 5 minutes.
	ProducerTimeout time.Duration
	// IdleTimeout cancels a producer that emits nothing for this long —
	// catches upstream connections that hang without returning bytes.
	// Default 90 seconds; negative disables.
	IdleTimeout time.Duration
	// KeepAlive is the tail poll timeout and keepalive interval. Default
	// 20 seconds.
	KeepAlive time.Duration
	// FinalizeTimeout bounds the terminal writes (error event, Finish,
	// OnFinish) that run on a fresh context. Default 10 seconds.
	FinalizeTimeout time.Duration
	// MaxBodyBytes caps the request body. Default 1 MiB.
	MaxBodyBytes int64
	// IsResume decides whether a POST body is a pure resume (tail only, no
	// producer). Default: empty, "{}" or "null".
	IsResume func(body []byte) bool
	// ProducerContext returns the base context the detached producer runs
	// in. Use it to stamp request-scoped values (user id, trace id) that
	// tools need. Default context.Background(). When the returned context
	// carries no span context, the request's own span context is added to
	// it, so the run's spans join the request's trace.
	ProducerContext func(r *http.Request) context.Context
	// OnFinish is called once per producer run, on a fresh context, after
	// the terminal event is in the buffer and before the stream is marked
	// finished, so what it persists is in place when a client's stream
	// ends.
	OnFinish func(ctx context.Context, r Result)
	// FriendlyError maps a producer error to the message clients see.
	// Default DefaultFriendlyError.
	FriendlyError func(error) string
	// NewMessageID mints an id when the client sends none. Default: 32
	// random hex chars.
	NewMessageID func() string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (o *Options) defaults() {
	if o.ProducerTimeout <= 0 {
		o.ProducerTimeout = 5 * time.Minute
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 90 * time.Second
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 20 * time.Second
	}
	if o.FinalizeTimeout <= 0 {
		o.FinalizeTimeout = 10 * time.Second
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1 << 20
	}
	if o.IsResume == nil {
		o.IsResume = DefaultIsResume
	}
	if o.ProducerContext == nil {
		o.ProducerContext = func(*http.Request) context.Context { return context.Background() }
	}
	if o.FriendlyError == nil {
		o.FriendlyError = DefaultFriendlyError
	}
	if o.NewMessageID == nil {
		o.NewMessageID = NewMessageID
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// DefaultIsResume treats an empty body, "{}" or "null" as a pure tail.
func DefaultIsResume(body []byte) bool {
	s := strings.TrimSpace(string(body))
	return s == "" || s == "{}" || s == "null"
}

// NewMessageID returns 32 random hex characters.
func NewMessageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DefaultFriendlyError maps internal errors to one actionable line. Raw
// provider errors must not reach end users verbatim: they leak internals
// and read as a broken product. The full detail is logged server-side.
func DefaultFriendlyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "The model took too long to respond. Please try again."
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "timeout") || strings.Contains(s, "http2:"):
		return "The AI service did not respond in time. Please try again."
	case strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected EOF") || strings.Contains(s, "use of closed network connection"):
		return "The connection to the AI service was interrupted. Please try again."
	case strings.Contains(s, "429") || strings.Contains(strings.ToLower(s), "rate limit"):
		return "The AI service is busy right now. Please try again in a moment."
	}
	return "The AI service is temporarily unavailable. Please try again in a moment."
}

// Handler is the resumable stream endpoint.
type Handler struct {
	store    streambuf.Store
	produce  Producer
	opts     Options
	inflight sync.Map // msgID -> *producerCtl
}

// producerCtl shares cancellation state between a producer goroutine and
// Cancel. userCancelled is set when a client explicitly stops the stream so
// the producer can distinguish an intentional stop from a timeout.
type producerCtl struct {
	cancel        context.CancelFunc
	userCancelled atomic.Bool
	// launch is the span context the producer runs under; a request that
	// tails this producer links to it.
	launch trace.SpanContext
}

// NewHandler builds a Handler over store and produce.
func NewHandler(store streambuf.Store, produce Producer, opts Options) *Handler {
	if store == nil {
		panic("sse: NewHandler needs a streambuf.Store")
	}
	if produce == nil {
		panic("sse: NewHandler needs a Producer")
	}
	opts.defaults()
	return &Handler{store: store, produce: produce, opts: opts}
}

// ServeHTTP handles POST (first connect or resume) and GET (pure tail).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	switch r.Method {
	case http.MethodPost:
		b, err := io.ReadAll(io.LimitReader(r.Body, h.opts.MaxBodyBytes+1))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(b)) > h.opts.MaxBodyBytes {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	case http.MethodGet:
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	msgID := strings.TrimSpace(q.Get("message_id"))
	isResume := r.Method == http.MethodGet || h.opts.IsResume(body)
	if msgID == "" {
		if isResume {
			http.Error(w, "message_id required to resume", http.StatusBadRequest)
			return
		}
		msgID = h.opts.NewMessageID()
	}
	since := 0
	if v := q.Get("since"); v != "" {
		since, _ = strconv.Atoi(v)
	} else if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ = strconv.Atoi(v)
	}
	if since < 0 {
		since = 0
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	hdr.Set("X-Message-Id", msgID)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	reqSpan := trace.SpanFromContext(r.Context())
	if reqSpan.IsRecording() {
		reqSpan.SetAttributes(telemetry.MessageID.String(msgID))
	}
	launched := false
	if !isResume {
		acquired, err := h.Launch(producerContext(h.opts.ProducerContext(r), r), msgID, body)
		if err != nil {
			h.opts.Logger.Error("stream start failed", "err", err, "message_id", msgID)
			writeFrame(w, 0, agent.StreamEvent{Type: agent.EventError, Message: "service temporarily unavailable — please retry"})
			flush()
			return
		}
		// Not acquired means a producer already owns this id (a retried
		// connect) — just tail it.
		launched = acquired
	}
	if !launched && reqSpan.IsRecording() {
		if v, ok := h.inflight.Load(msgID); ok {
			if ctl, _ := v.(*producerCtl); ctl != nil && ctl.launch.IsValid() {
				reqSpan.AddLink(trace.Link{SpanContext: ctl.launch})
			}
		}
	}
	h.tail(w, r, msgID, since, flush)
}

// producerContext adds the request's span context to base unless base
// already carries one. The span context is attached as a local parent
// (trace.ContextWithSpanContext), not the live span: the request span ends
// when the connection does, while the run it launched keeps going, and a
// ParentBased sampler applies its local-parent rule to a span of this
// process.
func producerContext(base context.Context, r *http.Request) context.Context {
	if base == nil {
		base = context.Background()
	}
	if trace.SpanContextFromContext(base).IsValid() {
		return base
	}
	if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
		return trace.ContextWithSpanContext(base, sc)
	}
	return base
}

// Launch claims msgID in the store and, if this caller won, starts the
// producer in a detached goroutine. acquired=false means another producer
// already owns the id; callers should tail. Applications use it to kick
// off a continuation turn (for example after resolving a
// human-in-the-loop pause) and then let the client tail by message id.
func (h *Handler) Launch(base context.Context, msgID string, body []byte) (acquired bool, err error) {
	acquired, err = h.store.Start(context.Background(), msgID)
	if err != nil || !acquired {
		return acquired, err
	}
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, h.opts.ProducerTimeout)
	ctl := &producerCtl{cancel: cancel, launch: trace.SpanContextFromContext(base)}
	// Belt and braces: even if a store ever allowed a double Start, only
	// one producer goroutine spawns per id per process.
	if _, loaded := h.inflight.LoadOrStore(msgID, ctl); loaded {
		cancel()
		return false, nil
	}
	go h.run(ctx, ctl, msgID, body)
	return true, nil
}

// Cancel stops an in-flight producer. Idempotent: false when no producer
// for msgID is running in this process. Partial output already in the
// buffer is finalised as a normal, cancelled "done".
func (h *Handler) Cancel(msgID string) bool {
	v, ok := h.inflight.Load(msgID)
	if !ok {
		return false
	}
	ctl, _ := v.(*producerCtl)
	if ctl == nil {
		return false
	}
	ctl.userCancelled.Store(true)
	ctl.cancel()
	h.opts.Logger.Info("stream cancel requested", "message_id", msgID)
	return true
}

// CancelHandler serves POST ?message_id=<id> and replies
// {"ok":true,"cancelled":<bool>}.
func (h *Handler) CancelHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		msgID := strings.TrimSpace(r.URL.Query().Get("message_id"))
		if msgID == "" {
			http.Error(w, "message_id required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "cancelled": h.Cancel(msgID)})
	})
}

// tail forwards buffered events to one connection until the stream ends or
// the client goes away.
func (h *Handler) tail(w http.ResponseWriter, r *http.Request, msgID string, since int, flush func()) {
	started := time.Now()
	for {
		events, status, err := h.store.Tail(r.Context(), msgID, since, h.opts.KeepAlive)
		if err != nil {
			// Client disconnected or the store failed; the producer keeps
			// running and the client can resume with since=<last id>.
			h.opts.Logger.Debug("stream tail exit", "message_id", msgID, "since", since, "err", err,
				"duration_ms", time.Since(started).Milliseconds())
			return
		}
		for _, ev := range events {
			var peek struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &peek)
			if peek.Type == "" {
				peek.Type = "message"
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, peek.Type, ev.Data)
			since = ev.Seq
		}
		flush()
		if status != "" {
			return
		}
		fmt.Fprint(w, ": ping\n\n")
		flush()
	}
}

func writeFrame(w io.Writer, seq int, ev agent.StreamEvent) {
	b, _ := json.Marshal(ev)
	if seq > 0 {
		fmt.Fprintf(w, "id: %d\n", seq)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
}

// run owns a single producer for msgID. Three triggers bound it: the
// producer timeout, the idle watchdog, and Cancel. Finalisation (terminal
// event, Finish, OnFinish) runs on a fresh short-lived context so it lands
// even when the primary context has already expired.
func (h *Handler) run(ctx context.Context, ctl *producerCtl, msgID string, body []byte) {
	defer h.inflight.Delete(msgID)
	defer ctl.cancel()
	log := h.opts.Logger

	var acc agent.Accumulator
	var lastEventNs atomic.Int64
	lastEventNs.Store(time.Now().UnixNano())

	append_ := func(c context.Context, ev agent.StreamEvent) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if err := h.store.Append(c, msgID, string(b)); err != nil {
			log.Warn("stream append", "err", err, "message_id", msgID, "type", ev.Type)
		}
	}

	finish := func(status string, res Result) {
		fctx, cancel := context.WithTimeout(context.Background(), h.opts.FinalizeTimeout)
		defer cancel()
		res.MessageID = msgID
		res.Turns = acc.Turns
		res.Status = status
		res.Paused, res.PausedKind, res.PausedToolUseID = acc.Paused, acc.PausedKind, acc.PausedToolUseID
		res.PausedToolName, res.PausedToolInput, res.PausedPrompt = acc.PausedToolName, acc.PausedToolInput, acc.PausedPrompt
		res.PausedRequest = acc.PausedRequest
		res.Usage = Usage{acc.InputTokens, acc.OutputTokens, acc.CacheReadTokens, acc.CacheCreationTokens}
		res.EmittedText = acc.Text.Len() > 0
		if res.Err != nil && !res.Cancelled {
			append_(fctx, agent.StreamEvent{Type: agent.EventError, Message: h.opts.FriendlyError(res.Err)})
		}
		if res.Cancelled {
			append_(fctx, agent.StreamEvent{Type: agent.EventDone, Message: "cancelled"})
		}
		// Persist before the stream is marked finished: a client that sees
		// its stream end and reloads must find the turns and any pending
		// pause already stored.
		if h.opts.OnFinish != nil {
			h.opts.OnFinish(fctx, res)
		}
		if err := h.store.Finish(fctx, msgID, status); err != nil {
			log.Warn("stream finish", "err", err, "message_id", msgID, "status", status)
		}
	}

	// A panicking producer must still leave a terminal event behind, or
	// the client stalls on keepalives until the buffer expires.
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("stream producer panic", "panic", rec, "message_id", msgID)
			err := fmt.Errorf("internal error: %v", rec)
			finish(streambuf.ErrorStatus("internal error"), Result{Err: err})
		}
	}()

	emit := func(ev agent.StreamEvent) {
		acc.Observe(ev)
		lastEventNs.Store(time.Now().UnixNano())
		// Turn events are an internal provider→server boundary signal used
		// only to build the accumulator; keep them off the wire.
		if ev.Type == agent.EventTurn {
			return
		}
		// present_data is a pure pass-through: hide its spinner and turn
		// its tool_result into the gen_ui event the client renders.
		if ev.Name == tool.PresentDataToolName {
			switch ev.Type {
			case agent.EventToolCall:
				return
			case agent.EventToolResult:
				append_(ctx, agent.StreamEvent{Type: agent.EventGenUI, ToolUseID: ev.ToolUseID, GenUI: ev.Input})
				return
			}
		}
		// Any tool whose envelope carries ui_payload renders too; the
		// tool_result still flows so the trace shows the observation.
		if ev.Type == agent.EventToolResult && len(ev.Output) > 0 {
			if ui := tool.ExtractUIPayload(json.RawMessage(ev.Output)); len(ui) > 0 {
				append_(ctx, agent.StreamEvent{Type: agent.EventGenUI, ToolUseID: ev.ToolUseID, Name: ev.Name, GenUI: ui})
			}
			// runtime_state is the server's; the client gets the rest.
			ev.Output = string(tool.ClientView(json.RawMessage(ev.Output)))
		}
		append_(ctx, ev)
	}

	// Idle watchdog.
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		if h.opts.IdleTimeout < 0 {
			return
		}
		tick := h.opts.IdleTimeout / 6
		if tick < 10*time.Millisecond {
			tick = 10 * time.Millisecond
		}
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if now.UnixNano()-lastEventNs.Load() > int64(h.opts.IdleTimeout) {
					log.Warn("stream producer idle timeout — cancelling", "message_id", msgID, "idle", h.opts.IdleTimeout)
					ctl.cancel()
					return
				}
			}
		}
	}()

	emit(agent.StreamEvent{Type: agent.EventMeta, MessageID: msgID})
	started := time.Now()
	err := h.produce(ctx, msgID, body, emit)
	ctl.cancel()
	<-watchdogDone

	cancelled := ctl.userCancelled.Load()
	switch {
	case cancelled:
		log.Info("stream producer cancelled by user", "message_id", msgID, "duration_ms", time.Since(started).Milliseconds())
		finish(streambuf.StatusDone, Result{Cancelled: true})
	case err != nil:
		log.Error("stream producer failed", "err", err, "message_id", msgID, "duration_ms", time.Since(started).Milliseconds())
		finish(streambuf.ErrorStatus(h.opts.FriendlyError(err)), Result{Err: err})
	case acc.Paused:
		log.Info("stream producer paused for user input", "message_id", msgID, "kind", acc.PausedKind, "tool_use_id", acc.PausedToolUseID)
		finish(streambuf.StatusWaiting, Result{})
	default:
		log.Info("stream producer done", "message_id", msgID, "duration_ms", time.Since(started).Milliseconds(),
			"input_tokens", acc.InputTokens, "output_tokens", acc.OutputTokens)
		finish(streambuf.StatusDone, Result{})
	}
}
