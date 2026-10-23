// Command chatserver is a runnable reference deployment of dialogram: a
// travel-assistant chat with resumable streaming, inline charts and tables,
// human-in-the-loop choices, an approval-gated tool and an in-memory
// conversation store. It is deliberately small — read it top to bottom to
// see how the pieces fit, then replace the store and the tools with yours.
//
//	ANTHROPIC_API_KEY=… go run ./examples/chatserver
//	OPENAI_API_KEY=… LLM_PROVIDER=openai go run ./examples/chatserver
//
// Optional: REDIS_URL (multi-instance stream buffer), PORT (default 8080),
// GUARD=1 (enable the honesty guard; it trades token streaming for
// redaction at turn boundaries).
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/sse"
	"github.com/Cyprinus12138/dialogram/streambuf"
	"github.com/Cyprinus12138/dialogram/tool"
)

//go:embed static/*
var static embed.FS

const systemPrompt = `You are a concise travel assistant. You help with weather, flights and bookings for a trip.

Rules:
- Only state figures (temperatures, fares, times) that a tool result supplies. If a tool fails or returns nothing, say so and offer a next step. Never estimate from memory.
- When the user's destination or date is ambiguous, ask with request_user_choice (or request_user_input) instead of guessing.
- After search_flights returns an artifact ref, show the options with render_from_ref (comparison_table) rather than retyping rows. Use present_data for small summaries (metric_cards, bar_chart).
- book_flight needs the user's approval; call it when the user has picked a flight.
- Reply in the user's language. Keep answers short.`

// ---------------- demo tools ----------------

type forecastDay struct {
	Day    string
	HighC  float64
	LowC   float64
	Rain   int
	Skies  string
	Source string
}

// demoForecast is deterministic fake weather so the example runs offline.
func demoForecast(city string) []forecastDay {
	seed := 0
	for _, r := range strings.ToLower(city) {
		seed += int(r)
	}
	days := []string{"Mon", "Tue", "Wed", "Thu", "Fri"}
	out := make([]forecastDay, 0, len(days))
	for i, d := range days {
		high := 18 + float64((seed+i*7)%12)
		out = append(out, forecastDay{Day: d, HighC: high, LowC: high - 8, Rain: (seed + i*13) % 60,
			Skies: []string{"sunny", "cloudy", "showers"}[(seed+i)%3], Source: "demo weather service"})
	}
	return out
}

func weatherTool() *tool.Tool {
	return &tool.Tool{
		Name:        "get_weather",
		Label:       "Weather",
		Description: "Five-day forecast for a city: daily high/low in °C, rain chance and skies. Returns a bar chart of the highs for the user and an artifact ref of the full table.",
		Schema:      tool.Obj(map[string]any{"city": tool.Str("City name")}, "city"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in struct {
				City string `json:"city"`
			}
			_ = json.Unmarshal(input, &in)
			if strings.TrimSpace(in.City) == "" {
				return tool.SoftEnvelope(tool.ErrorCodeInvalidInput, "city is required"), nil
			}
			days := demoForecast(in.City)
			obs := make([]map[string]any, 0, len(days))
			bars := make([]map[string]any, 0, len(days))
			rows := make([][]any, 0, len(days))
			for _, d := range days {
				obs = append(obs, map[string]any{"day": d.Day, "high_c": d.HighC, "low_c": d.LowC, "rain_chance_pct": d.Rain, "skies": d.Skies})
				bars = append(bars, map[string]any{"label": d.Day, "value": d.HighC})
				rows = append(rows, []any{d.Day, d.HighC, d.LowC, d.Rain, d.Skies})
			}
			id := tool.ToolUseIDFromContext(ctx)
			return &tool.Envelope{
				ModelObservation: map[string]any{"city": in.City, "days": obs, "source": tool.Source{Dataset: "demo weather service", RetrievedAt: time.Now().UTC().Format(time.RFC3339)}},
				UIPayload: &tool.UIPayload{Component: tool.ComponentBarChart, Data: map[string]any{
					"type": tool.ComponentBarChart, "title": "Daily high, " + in.City + " (°C)", "data": bars, "y_label": "°C",
				}},
				RuntimeState: &tool.TableRecord{
					Columns: []tool.TableColumn{{Key: "day", Label: "Day"}, {Key: "high_c", Label: "High °C", Type: "number"}, {Key: "low_c", Label: "Low °C", Type: "number"}, {Key: "rain_pct", Label: "Rain %", Type: "number"}, {Key: "skies", Label: "Skies"}},
					Rows:    rows,
				},
				Artifacts: []tool.ArtifactRef{{ID: id, Type: tool.ShapeTableRecord, Description: "full 5-day forecast table"}},
			}, nil
		},
	}
}

type flight struct {
	Carrier, Departs, Arrives string
	Fare                      float64
	Stops                     int
}

func demoFlights(from, to string) []flight {
	seed := len(from)*31 + len(to)*17
	carriers := []string{"SKYLINE", "IBERWING", "AEROVISTA", "NORTHARC"}
	out := make([]flight, 0, 4)
	for i, c := range carriers {
		h := 6 + (seed+i*3)%14
		out = append(out, flight{Carrier: c, Departs: fmt.Sprintf("%02d:%02d", h, (seed*i)%60), Arrives: fmt.Sprintf("%02d:%02d", (h+2)%24, (seed*i+25)%60),
			Fare: float64(60 + (seed*(i+1))%240), Stops: i % 2})
	}
	return out
}

func flightsTool() *tool.Tool {
	return &tool.Tool{
		Name:        "search_flights",
		Label:       "Flight search",
		Description: "Search flights between two cities on a date. Returns a short summary and an artifact ref of the full result table; use render_from_ref to show it.",
		Schema: tool.Obj(map[string]any{
			"from": tool.Str("Origin city or airport code"),
			"to":   tool.Str("Destination city or airport code"),
			"date": tool.Str("Travel date, YYYY-MM-DD"),
		}, "from", "to"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in struct{ From, To, Date string }
			_ = json.Unmarshal(input, &in)
			if in.From == "" || in.To == "" {
				return tool.SoftEnvelope(tool.ErrorCodeInvalidInput, "from and to are required"), nil
			}
			fl := demoFlights(in.From, in.To)
			rows := make([][]any, 0, len(fl))
			summary := make([]map[string]any, 0, len(fl))
			cheapest := fl[0]
			for _, f := range fl {
				rows = append(rows, []any{f.Carrier, f.Departs, f.Arrives, f.Stops, f.Fare})
				summary = append(summary, map[string]any{"carrier": f.Carrier, "departs": f.Departs, "fare_eur": f.Fare, "stops": f.Stops})
				if f.Fare < cheapest.Fare {
					cheapest = f
				}
			}
			id := tool.ToolUseIDFromContext(ctx)
			return &tool.Envelope{
				ModelObservation: map[string]any{
					"from": in.From, "to": in.To, "date": in.Date, "n": len(fl),
					"cheapest": map[string]any{"carrier": cheapest.Carrier, "fare_eur": cheapest.Fare, "departs": cheapest.Departs},
					"flights":  summary,
					"source":   tool.Source{Dataset: "demo fares", RetrievedAt: time.Now().UTC().Format(time.RFC3339)},
				},
				RuntimeState: &tool.TableRecord{
					Columns: []tool.TableColumn{{Key: "carrier", Label: "Carrier"}, {Key: "departs", Label: "Departs"}, {Key: "arrives", Label: "Arrives"}, {Key: "stops", Label: "Stops", Type: "number"}, {Key: "fare_eur", Label: "Fare (EUR)", Type: "currency"}},
					Rows:    rows,
				},
				Artifacts: []tool.ArtifactRef{{ID: id, Type: tool.ShapeTableRecord, Description: fmt.Sprintf("%d flights %s→%s", len(fl), in.From, in.To)}},
			}, nil
		},
	}
}

func bookTool() *tool.Tool {
	return &tool.Tool{
		Name:        "book_flight",
		Label:       "Book flight",
		Description: "Book a flight for the user. Requires the user's approval before it runs.",
		Schema:      tool.Obj(map[string]any{"carrier": tool.Str("Carrier"), "departs": tool.Str("Departure time"), "fare_eur": tool.Num("Fare in EUR")}, "carrier", "departs"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in map[string]any
			_ = json.Unmarshal(input, &in)
			return map[string]any{"booked": true, "confirmation": "DG-" + strings.ToUpper(agent.NewToolUseID()[3:9]), "details": in}, nil
		},
	}
}

// ---------------- conversation store ----------------

type pending struct {
	ToolUseID string          `json:"tool_use_id"`
	Kind      string          `json:"kind"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
}

type conversation struct {
	ID      string       `json:"id"`
	Turns   []agent.Turn `json:"turns"`
	Pending *pending     `json:"pending,omitempty"`
}

type store struct {
	mu    sync.Mutex
	convs map[string]*conversation
	byMsg map[string]string // message id → conversation id
}

func newStore() *store { return &store{convs: map[string]*conversation{}, byMsg: map[string]string{}} }

func (s *store) get(id string) (*conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[id]
	return c, ok
}

func (s *store) getOrCreate(id string) *conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = sse.NewMessageID()[:12]
	}
	c, ok := s.convs[id]
	if !ok {
		c = &conversation{ID: id}
		s.convs[id] = c
	}
	return c
}

func (s *store) snapshot(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[id]
	if !ok {
		return conversation{}, false
	}
	cp := *c
	cp.Turns = append([]agent.Turn(nil), c.Turns...)
	return cp, true
}

// ---------------- server ----------------

type streamBody struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
	Continue       bool   `json:"continue"`
}

type respondBody struct {
	ConversationID string                     `json:"conversation_id"`
	ToolUseID      string                     `json:"tool_use_id"`
	MessageID      string                     `json:"message_id"`
	Response       string                     `json:"response"`
	Selected       []string                   `json:"selected"`
	OtherText      string                     `json:"other_text"`
	Answers        map[string]json.RawMessage `json:"answers"`
	Approved       *bool                      `json:"approved"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	reg := tool.NewRegistry()
	tool.RegisterHITL(reg)
	tool.RegisterPresentData(reg)
	tool.RegisterRenderFromRef(reg)
	tool.RegisterNow(reg, time.UTC)
	reg.Register(weatherTool())
	reg.Register(flightsTool())
	reg.Register(bookTool())

	cfg := &agent.Config{
		Profiles:       map[string]agent.Profile{"assistant": {SystemPrompt: systemPrompt, MaxToolSteps: 8}},
		DefaultProfile: "assistant",
	}
	deps := dialogram.Deps{
		Registry:     reg,
		Config:       cfg,
		Interceptors: []agent.Interceptor{agent.NewApprovalListInterceptor([]string{"book_flight"})},
	}
	if os.Getenv("GUARD") != "" {
		deps.Guard = agent.NewGuard("get_weather", "search_flights")
	}
	ag, err := dialogram.New(dialogram.SettingsFromEnv(), deps)
	if err != nil {
		slog.Error("configure provider", "err", err)
		os.Exit(1)
	}

	var buf streambuf.Store
	if url := os.Getenv("REDIS_URL"); url != "" {
		rs, err := streambuf.NewRedisStore(context.Background(), streambuf.RedisOptions{URL: url})
		if err != nil {
			slog.Warn("redis unavailable — falling back to in-memory stream buffer", "err", err)
			buf = streambuf.NewMemStore(0)
		} else {
			buf = rs
		}
	} else {
		buf = streambuf.NewMemStore(0)
	}

	db := newStore()

	produce := func(ctx context.Context, msgID string, body []byte, emit agent.EmitFn) error {
		var req streamBody
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("bad request body: %w", err)
		}
		conv := db.getOrCreate(req.ConversationID)
		db.mu.Lock()
		db.byMsg[msgID] = conv.ID
		if !req.Continue {
			if strings.TrimSpace(req.Message) == "" {
				db.mu.Unlock()
				return errors.New("message is required")
			}
			conv.Turns = append(conv.Turns, agent.UserTurn(req.Message))
		}
		history := append([]agent.Turn(nil), conv.Turns...)
		db.mu.Unlock()
		// Second meta frame: the conversation id, so a client that created a
		// fresh conversation learns it from the stream itself.
		emit(agent.StreamEvent{Type: agent.EventMeta, Message: conv.ID, Provider: ag.Provider(), Model: ag.Model()})
		return ag.ChatStream(ctx, agent.ChatInput{Turns: history, Profile: "assistant"}, emit)
	}

	onFinish := func(_ context.Context, r sse.Result) {
		db.mu.Lock()
		defer db.mu.Unlock()
		convID := db.byMsg[r.MessageID]
		delete(db.byMsg, r.MessageID)
		conv, ok := db.convs[convID]
		if !ok {
			return
		}
		conv.Turns = append(conv.Turns, r.Turns...)
		conv.Pending = nil
		if r.Paused {
			conv.Pending = &pending{ToolUseID: r.PausedToolUseID, Kind: r.PausedKind, ToolName: r.PausedToolName, ToolInput: r.PausedToolInput, Prompt: r.PausedPrompt}
		}
	}

	handler := sse.NewHandler(buf, produce, sse.Options{OnFinish: onFinish})

	mux := http.NewServeMux()
	mux.Handle("/chat/stream", handler)
	mux.Handle("/chat/cancel", handler.CancelHandler())
	mux.HandleFunc("/chat/respond", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req respondBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		conv, ok := db.get(req.ConversationID)
		if !ok {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		db.mu.Lock()
		p := conv.Pending
		if p == nil || p.ToolUseID != req.ToolUseID {
			db.mu.Unlock()
			http.Error(w, "no matching pending interaction", http.StatusConflict)
			return
		}
		db.mu.Unlock()

		// Build the tool_result the model will read on resume.
		var result json.RawMessage
		switch p.Kind {
		case agent.PauseKindApproval:
			if req.Approved == nil {
				http.Error(w, "approved is required", http.StatusBadRequest)
				return
			}
			if *req.Approved {
				out, err := reg.Call(r.Context(), p.ToolName, string(p.ToolInput))
				if err != nil {
					out = fmt.Sprintf(`{"error_code":%q,"error_message":%q}`, tool.ErrorCodeHardFailure, err.Error())
				}
				result = json.RawMessage(out)
			} else {
				reason := strings.TrimSpace(req.Response)
				if reason == "" {
					reason = "The user declined to run this tool."
				}
				result = json.RawMessage(agent.DeniedResultJSON(reason))
			}
		case agent.PauseKindHITLChoice:
			if len(req.Selected) == 0 {
				http.Error(w, "selected is required", http.StatusBadRequest)
				return
			}
			result, _ = json.Marshal(map[string]any{"selected": req.Selected, "other_text": req.OtherText})
		case agent.PauseKindHITLForm:
			if len(req.Answers) == 0 {
				http.Error(w, "answers is required", http.StatusBadRequest)
				return
			}
			result, _ = json.Marshal(map[string]any{"answers": req.Answers})
		default:
			result, _ = json.Marshal(map[string]any{"user_response": req.Response})
		}
		db.mu.Lock()
		conv.Turns = append(conv.Turns, agent.Turn{Role: agent.RoleTool, Blocks: []agent.Block{{
			Type: agent.BlockToolResult, ToolUseID: p.ToolUseID, ToolName: p.ToolName, ToolResult: result,
		}}})
		conv.Pending = nil
		db.mu.Unlock()

		msgID := strings.TrimSpace(req.MessageID)
		if msgID == "" {
			msgID = sse.NewMessageID()
		}
		body, _ := json.Marshal(streamBody{ConversationID: conv.ID, Continue: true})
		if _, err := handler.Launch(context.Background(), msgID, body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "conversation_id": conv.ID, "message_id": msgID})
	})
	mux.HandleFunc("/conversations/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/conversations/")
		c, ok := db.snapshot(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
	})
	sub, _ := fs.Sub(static, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	slog.Info("chatserver listening", "addr", "http://localhost:"+port, "provider", ag.Provider(), "model", ag.Model(), "guard", deps.Guard != nil)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
