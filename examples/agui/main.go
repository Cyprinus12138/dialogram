// Command agui serves a weather assistant over the AG-UI protocol, for
// CopilotKit-style frontends: POST a RunAgentInput to /agui and read the
// event stream back. See docs/AGUI.md for the mapping and the interrupt
// flow.
//
//	ANTHROPIC_API_KEY=… go run ./examples/agui
//	curl -N localhost:8080/agui -H 'Content-Type: application/json' -d '{"threadId":"t1","runId":"r1",
//	  "messages":[{"id":"u1","role":"user","content":"Weather in Lisbon? Book a table at Ramiro for 20:00 if it is dry."}]}'
//
// Booking needs the user's approval, so that run ends with RUN_FINISHED
// carrying an interrupt. Answer it with a new run on the same thread: the
// messages of the MESSAGES_SNAPSHOT before it, and a resume entry.
//
//	curl -N localhost:8080/agui -H 'Content-Type: application/json' -d '{"threadId":"t1","runId":"r2",
//	  "messages":[…the snapshot messages…],
//	  "resume":[{"interruptId":"dgi1.…","status":"resolved","payload":{"approved":true}}]}'
//
// Optional: AGUI_INTERRUPT_KEY, the key interrupt ids are signed with.
// Without it a random key is used, and interrupts do not survive a restart.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/agui"
	"github.com/Cyprinus12138/dialogram/tool"
)

// weatherTool is a deterministic fake forecast so the example runs offline:
// the model hears the numbers, the user sees a bar chart (CUSTOM gen_ui).
func weatherTool() *tool.Tool {
	return &tool.Tool{
		Name:        "get_weather",
		Label:       "Weather",
		Description: "Five-day forecast for a city: daily high in °C. Returns a bar chart for the user.",
		Schema:      tool.Obj(map[string]any{"city": tool.Str("City name")}, "city"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in struct {
				City string `json:"city"`
			}
			_ = json.Unmarshal(input, &in)
			if strings.TrimSpace(in.City) == "" {
				return tool.SoftEnvelope(tool.ErrorCodeInvalidInput, "city is required"), nil
			}
			seed := 0
			for _, r := range strings.ToLower(in.City) {
				seed += int(r)
			}
			var highs []map[string]any
			for i, d := range []string{"Mon", "Tue", "Wed", "Thu", "Fri"} {
				highs = append(highs, map[string]any{"label": d, "value": 18 + (seed+i*7)%12})
			}
			return &tool.Envelope{
				ModelObservation: map[string]any{"city": in.City, "highs_c": highs, "source": "demo weather service"},
				UIPayload:        &tool.UIPayload{Component: tool.ComponentBarChart, Data: map[string]any{"type": tool.ComponentBarChart, "title": "Daily high, " + in.City + " (°C)", "data": highs}},
			}, nil
		},
	}
}

// bookTool is a fake restaurant booking. It is annotated destructive, so
// the policy interceptor pauses it for the user's approval.
func bookTool() *tool.Tool {
	return &tool.Tool{
		Name:        "book_table",
		Label:       "Book a table",
		Description: "Book a restaurant table for the user. The user approves the booking before it runs.",
		Schema:      tool.Obj(map[string]any{"restaurant": tool.Str("Restaurant name"), "time": tool.Str("Time, HH:MM")}, "restaurant", "time"),
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in struct {
				Restaurant string `json:"restaurant"`
				Time       string `json:"time"`
			}
			_ = json.Unmarshal(input, &in)
			return map[string]any{"booked": true, "restaurant": in.Restaurant, "time": in.Time, "reference": "DEMO-1234"}, nil
		},
		Annotations: tool.Annotations{Destructive: true},
	}
}

func main() {
	reg := tool.NewRegistry()
	tool.RegisterHITL(reg)
	reg.Register(weatherTool())
	reg.Register(bookTool())
	cfg := &agent.Config{
		Profiles: map[string]agent.Profile{"assistant": {MaxToolSteps: 6, SystemPrompt: "You are a concise travel assistant. " +
			"Only state figures a tool returned. When the city is ambiguous, ask with request_user_choice instead of guessing. " +
			"Book with book_table when the user asks for it."}},
		DefaultProfile: "assistant",
	}
	// One executor for the agent's tool calls and for the calls the user
	// approves on resume, so both get the same timeouts and limits.
	exec := tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{})
	ag, err := dialogram.New(dialogram.SettingsFromEnv(), dialogram.Deps{
		Registry:     reg,
		Config:       cfg,
		Executor:     exec,
		Interceptors: []agent.Interceptor{agent.NewPolicyInterceptor(reg, agent.Policy{PauseDestructive: true})},
	})
	if err != nil {
		slog.Error("configure provider", "err", err)
		os.Exit(1)
	}

	// The client holds the history and re-sends it every run, so the server
	// keeps no conversation state: convert the messages and stream.
	run := func(ctx context.Context, in agui.RunAgentInput, emit agent.EmitFn) error {
		turns, err := agui.MessagesToTurns(in.Messages)
		if err != nil {
			return err
		}
		return ag.ChatStream(ctx, agent.ChatInput{Turns: turns, Profile: "assistant", MemoryContext: agui.SystemText(in.Messages)}, emit)
	}
	mux := http.NewServeMux()
	mux.Handle("/agui", agui.NewHandler(run, agui.Options{Executor: exec, InterruptKey: []byte(os.Getenv("AGUI_INTERRUPT_KEY"))}))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	slog.Info("agui example listening", "addr", "http://localhost:"+port+"/agui", "provider", ag.Provider(), "model", ag.Model())
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
