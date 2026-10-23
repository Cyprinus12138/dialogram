// Command agui serves a weather assistant over the AG-UI protocol, for
// CopilotKit-style frontends: POST a RunAgentInput to /agui and read the
// event stream back. See docs/AGUI.md for the mapping and the pause flow.
//
//	ANTHROPIC_API_KEY=… go run ./examples/agui
//	curl -N localhost:8080/agui -H 'Content-Type: application/json' -d '{"threadId":"t1","runId":"r1",
//	  "messages":[{"id":"u1","role":"user","content":"Weather in Lisbon?"}],"tools":[],"context":[],"forwardedProps":{}}'
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

func main() {
	reg := tool.NewRegistry()
	tool.RegisterHITL(reg)
	reg.Register(weatherTool())
	cfg := &agent.Config{
		Profiles: map[string]agent.Profile{"assistant": {MaxToolSteps: 6, SystemPrompt: "You are a concise travel assistant. " +
			"Only state figures a tool returned. When the city is ambiguous, ask with request_user_choice instead of guessing."}},
		DefaultProfile: "assistant",
	}
	ag, err := dialogram.New(dialogram.SettingsFromEnv(), dialogram.Deps{Registry: reg, Config: cfg})
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
	mux.Handle("/agui", agui.NewHandler(run, agui.Options{}))

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
