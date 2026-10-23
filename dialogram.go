// Package dialogram is the batteries-included entry point: pick a provider
// from settings, wrap it in an Agent, and you have a tool-using chat loop
// with streaming, human-in-the-loop pauses, inline generative UI and an
// optional honesty guard.
//
//	reg := tool.NewRegistry()
//	tool.RegisterHITL(reg)
//	tool.RegisterPresentData(reg)
//	reg.Register(myWeatherTool)
//
//	ag, err := dialogram.New(dialogram.SettingsFromEnv(), dialogram.Deps{
//		Registry: reg,
//		Config:   &agent.Config{Profiles: map[string]agent.Profile{"assistant": {SystemPrompt: "..."}}, DefaultProfile: "assistant"},
//	})
//
// The sub-packages are usable on their own: agent (block IR, profiles,
// guard), tool (registry + envelope), provider/anthropic and
// provider/openai (wire adapters), streambuf (resumable stream buffers),
// sse (the resumable HTTP handler) and eval (the evaluation harness).
package dialogram

import (
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/provider/anthropic"
	"github.com/Cyprinus12138/dialogram/provider/openai"
	"github.com/Cyprinus12138/dialogram/tool"
)

// Provider names accepted by Settings.Provider.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// Settings select and configure the model backend.
type Settings struct {
	// Provider is "anthropic" or "openai". Empty means anthropic when an
	// Anthropic key is set, else openai.
	Provider string

	AnthropicAPIKey  string
	AnthropicModel   string
	AnthropicBaseURL string

	OpenAIAPIKey  string
	OpenAIBaseURL string
	OpenAIModel   string
}

// SettingsFromEnv reads LLM_PROVIDER, ANTHROPIC_API_KEY, ANTHROPIC_MODEL,
// ANTHROPIC_BASE_URL, OPENAI_API_KEY, OPENAI_BASE_URL and OPENAI_MODEL.
func SettingsFromEnv() Settings {
	return Settings{
		Provider:         strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER"))),
		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:   os.Getenv("ANTHROPIC_MODEL"),
		AnthropicBaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:    os.Getenv("OPENAI_BASE_URL"),
		OpenAIModel:      os.Getenv("OPENAI_MODEL"),
	}
}

// Deps are the application-side pieces every provider receives.
type Deps struct {
	Registry     *tool.Registry
	Config       *agent.Config
	Interceptors []agent.Interceptor
	Skills       *agent.SkillLibrary
	// Executor runs tool calls; nil selects tool.NewLocalExecutor with the
	// default limits.
	Executor tool.Executor
	// Checkpointer, when non-nil, receives the run's block IR at every
	// step boundary so a crashed turn can be recovered (see
	// agent.Checkpoint and docs/STREAMING.md).
	Checkpointer agent.Checkpointer
	// Guard, Metrics and TracerProvider are layered on by New (not
	// NewProvider). A nil TracerProvider uses the global one, which records
	// nothing until an OpenTelemetry SDK is installed.
	Guard          *agent.Guard
	Metrics        *agent.Metrics
	TracerProvider trace.TracerProvider
}

// NewProvider builds the raw provider selected by s, WITHOUT the Agent
// wrapper — no guard, no metrics. Use New for production paths; this seam
// exists so an eval harness can observe the model's raw reply.
func NewProvider(s Settings, d Deps) (agent.Provider, error) {
	name := s.Provider
	if name == "" {
		if s.AnthropicAPIKey != "" {
			name = ProviderAnthropic
		} else {
			name = ProviderOpenAI
		}
	}
	switch name {
	case ProviderAnthropic:
		if s.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("dialogram: ANTHROPIC_API_KEY not set (provider=anthropic)")
		}
		return anthropic.New(anthropic.Options{
			APIKey: s.AnthropicAPIKey, Model: s.AnthropicModel, BaseURL: s.AnthropicBaseURL,
			Registry: d.Registry, Config: d.Config, Interceptors: d.Interceptors, Skills: d.Skills,
			Executor: d.Executor, Checkpointer: d.Checkpointer,
		}), nil
	case ProviderOpenAI:
		if s.OpenAIAPIKey == "" {
			return nil, fmt.Errorf("dialogram: OPENAI_API_KEY not set (provider=openai)")
		}
		return openai.New(openai.Options{
			APIKey: s.OpenAIAPIKey, BaseURL: s.OpenAIBaseURL, Model: s.OpenAIModel,
			Registry: d.Registry, Config: d.Config, Interceptors: d.Interceptors, Skills: d.Skills,
			Executor: d.Executor, Checkpointer: d.Checkpointer,
		}), nil
	default:
		return nil, fmt.Errorf("dialogram: unknown provider %q (want %q or %q)", name, ProviderAnthropic, ProviderOpenAI)
	}
}

// New builds the configured provider and wraps it in an Agent with the
// guard, metrics and tracer provider from d.
func New(s Settings, d Deps) (*agent.Agent, error) {
	p, err := NewProvider(s, d)
	if err != nil {
		return nil, err
	}
	return agent.New(p, agent.Options{Guard: d.Guard, Metrics: d.Metrics, TracerProvider: d.TracerProvider}), nil
}
