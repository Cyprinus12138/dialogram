// Command mcpagent is a terminal agent whose tools all come from one MCP
// server. It connects over streamable HTTP or stdio, registers the server's
// tools under a per-server prefix, adds the server's instructions to the
// system prompt, and asks on the terminal before any call the policy holds:
// a tool the server marks destructive or open-world, a tool it does not
// annotate at all, and any tool named with -approve. When the server asks
// the user for input in the middle of a call (MCP elicitation), the
// question is asked on the terminal too, field by field, for as many rounds
// as the server needs.
//
// Against sg-property-engine (https://github.com/Cyprinus12138/sg-property-engine)
// on its default port, over HTTP or by launching its stdio server:
//
//	OPENAI_API_KEY=… go run ./examples/mcpagent -url http://localhost:8081/mcp -name sgpe
//	OPENAI_API_KEY=… go run ./examples/mcpagent -name sgpe -- sgpe mcp --url http://localhost:8081
//
// Questions are read one per line from stdin, or given with -q (repeatable;
// the questions share one conversation and the program exits after the
// last). The model comes from the environment as in examples/chatserver:
// ANTHROPIC_API_KEY, or LLM_PROVIDER=openai with OPENAI_API_KEY,
// OPENAI_BASE_URL and OPENAI_MODEL for any OpenAI-compatible endpoint.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/tool"
	"github.com/Cyprinus12138/dialogram/tool/mcp"
)

const systemPrompt = `You are a helpful assistant. Your tools come from an MCP server.
Use them for every fact and figure you report, and say so when they return nothing.
When a call fails, read its error message: it usually says how to correct the call.`

type headerFlag struct{ h http.Header }

func (f *headerFlag) String() string { return "" }

func (f *headerFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("header %q is not Key: Value", v)
	}
	if f.h == nil {
		f.h = http.Header{}
	}
	f.h.Add(strings.TrimSpace(k), strings.TrimSpace(val))
	return nil
}

type listFlag []string

func (f *listFlag) String() string { return strings.Join(*f, " | ") }

func (f *listFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcpagent:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		url       = flag.String("url", "", "streamable HTTP endpoint (instead of a stdio command)")
		name      = flag.String("name", "", "server label and tool-name prefix (default: the server's advertised name)")
		approve   = flag.String("approve", "", "comma-separated registered tool names that always need approval")
		untrusted = flag.Bool("untrusted", false, "fence the server's results as data; use it for servers you do not operate")
		headers   headerFlag
		questions listFlag
	)
	flag.Var(&headers, "header", "extra HTTP header as \"Key: Value\" (repeatable; -url only)")
	flag.Var(&questions, "q", "question to ask (repeatable); without -q, questions are read from stdin")
	flag.Parse()

	var transport mcp.Transport
	switch {
	case *url != "":
		transport = mcp.StreamableHTTP{URL: *url, Header: headers.h}
	case flag.NArg() > 0:
		transport = mcp.Stdio{Command: flag.Arg(0), Args: flag.Args()[1:]}
	default:
		fmt.Fprintln(os.Stderr, "usage: mcpagent [flags] -url <endpoint>   or   mcpagent [flags] -- <command> [args...]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := mcp.Connect(ctx, transport, mcp.Options{Name: *name})
	if err != nil {
		return err
	}
	defer client.Close()

	s, err := newSession(ctx, dialogram.SettingsFromEnv(), client, options{approve: splitNames(*approve), untrusted: *untrusted}, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: %d tools, model %s\n", client.Name(), s.tools, s.ag.Model())

	if len(questions) > 0 {
		for _, q := range questions {
			fmt.Printf("> %s\n", q)
			if err := s.ask(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		fmt.Print("> ")
		q, ok := s.readLine()
		if !ok {
			fmt.Println()
			return nil
		}
		if q == "" {
			continue
		}
		if err := s.ask(ctx, q); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

// options are the policy knobs run takes from the flags.
type options struct {
	approve   []string // registered tool names that always pause for approval
	untrusted bool     // fence every result as data
}

// session is one conversation. in supplies the answers to approval prompts
// as well as the questions in interactive mode.
type session struct {
	ag    *agent.Agent
	exec  tool.Executor
	tools int
	in    *bufio.Scanner
	out   io.Writer
	turns []agent.Turn
}

func newSession(ctx context.Context, settings dialogram.Settings, c *mcp.Client, o options, in io.Reader, out io.Writer) (*session, error) {
	reg := tool.NewRegistry()
	// AssumeOpenWorld: a tool the server did not annotate counts as reaching
	// outside, so the policy below asks before running it.
	names, err := mcp.Register(ctx, reg, c, mcp.RegisterOptions{Untrusted: o.untrusted, AssumeOpenWorld: true})
	if err != nil {
		return nil, err
	}
	chain := []agent.Interceptor{agent.NewPolicyInterceptor(reg, agent.Policy{PauseDestructive: true, PauseOpenWorld: true, DenyUnknown: true})}
	if len(o.approve) > 0 {
		for _, n := range o.approve {
			if !slices.Contains(names, n) {
				return nil, fmt.Errorf("-approve: no tool named %q (registered: %s)", n, strings.Join(names, ", "))
			}
		}
		chain = append(chain, agent.NewApprovalListInterceptor(o.approve))
	}
	// The server's usage notes are meant for the model; tool/mcp leaves
	// placing them to the caller.
	prompt := systemPrompt
	if ins := strings.TrimSpace(c.Instructions()); ins != "" {
		prompt += "\n\n# " + c.Name() + "\n\n" + ins
	}
	// One executor for the loop's calls and the ones approved here, so both
	// get the same timeout and result cap.
	exec := tool.NewLocalExecutor(reg, tool.LocalExecutorOptions{})
	ag, err := dialogram.New(settings, dialogram.Deps{
		Registry: reg,
		Config: &agent.Config{
			Profiles:       map[string]agent.Profile{"mcp": {SystemPrompt: prompt, MaxToolSteps: 10}},
			DefaultProfile: "mcp",
		},
		Interceptors: chain,
		Executor:     exec,
	})
	if err != nil {
		return nil, err
	}
	return &session{ag: ag, exec: exec, tools: len(names), in: bufio.NewScanner(in), out: out}, nil
}

// ask runs one user message to the end of the turn, stopping for an answer
// whenever the run pauses for approval.
func (s *session) ask(ctx context.Context, q string) error {
	before := len(s.turns)
	s.turns = append(s.turns, agent.UserTurn(q))
	for {
		var acc agent.Accumulator
		err := s.ag.ChatStream(ctx, agent.ChatInput{Turns: s.turns}, func(ev agent.StreamEvent) {
			acc.Observe(ev)
			s.show(ev)
		})
		if err != nil {
			s.turns = s.turns[:before] // drop the failed exchange
			return err
		}
		s.turns = append(s.turns, acc.Turns...)
		if !acc.Paused {
			fmt.Fprintln(s.out)
			return nil
		}
		// The paused call, and every other call of its turn, gets a result
		// before the model is asked again.
		turns, err := agent.AnswerPause(s.turns, acc.PausedToolUseID, s.decide(ctx, &acc))
		if err != nil {
			s.turns = s.turns[:before]
			return err
		}
		s.turns = turns
	}
}

// decide answers an approval pause from the terminal: y runs the call
// through the shared executor, anything else declines it.
func (s *session) decide(ctx context.Context, acc *agent.Accumulator) json.RawMessage {
	switch acc.PausedKind {
	case agent.PauseKindApproval:
	case agent.PauseKindToolInput:
		return s.answerInput(ctx, acc)
	default:
		// Not reachable here: this agent registers no pause tools.
		return json.RawMessage(agent.DismissedResultJSON(acc.PausedKind, ""))
	}
	fmt.Fprintf(s.out, "\n  ? run %s %s [y/N] ", acc.PausedToolName, acc.PausedToolInput)
	answer, _ := s.readLine()
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return json.RawMessage(agent.DeniedResultJSON("The user declined to run this tool."))
	}
	out, err := s.exec.Execute(ctx, tool.Call{ToolUseID: acc.PausedToolUseID, Name: acc.PausedToolName, Input: acc.PausedToolInput})
	if err != nil {
		out = fmt.Sprintf(`{"error_code":%q,"error_message":%q}`, tool.ErrorCodeHardFailure, err.Error())
	}
	fmt.Fprintf(s.out, "  ← %s\n", observation(out))
	return json.RawMessage(out)
}

// answerInput asks the user what the server needs to finish the paused
// call, runs the call again with the answer, and repeats while the server
// asks again. The final result answers the call.
func (s *session) answerInput(ctx context.Context, acc *agent.Accumulator) json.RawMessage {
	var req tool.InputRequest
	_ = json.Unmarshal(acc.PausedRequest, &req)
	call := tool.Call{ToolUseID: acc.PausedToolUseID, Name: acc.PausedToolName, Input: acc.PausedToolInput}
	for {
		out, again, err := tool.ExecuteWithInput(ctx, s.exec, call, req, s.askUser(acc.PausedToolName, req))
		if err != nil {
			out = fmt.Sprintf(`{"error_code":%q,"error_message":%q}`, tool.ErrorCodeHardFailure, err.Error())
		}
		if again == nil {
			fmt.Fprintf(s.out, "  ← %s\n", observation(out))
			return json.RawMessage(out)
		}
		req = *again
	}
}

// askUser puts one input request to the user.
func (s *session) askUser(toolName string, req tool.InputRequest) tool.InputResponse {
	fmt.Fprintf(s.out, "\n  ? %s asks: %s\n", toolName, req.Message)
	if req.Mode == tool.InputModeURL {
		fmt.Fprintf(s.out, "    open %s\n    then type done, decline or cancel: ", req.URL)
		switch a, _ := s.readLine(); strings.ToLower(a) {
		case "done", "y", "yes":
			return tool.InputResponse{Action: tool.InputAccept}
		case "decline", "n", "no":
			return tool.InputResponse{Action: tool.InputDecline}
		}
		return tool.InputResponse{Action: tool.InputCancel}
	}
	fmt.Fprint(s.out, "    answer it? [Y]es, [n]o to decline, [c]ancel: ")
	switch a, ok := s.readLine(); {
	case !ok, strings.EqualFold(a, "c"), strings.EqualFold(a, "cancel"):
		return tool.InputResponse{Action: tool.InputCancel}
	case strings.EqualFold(a, "n"), strings.EqualFold(a, "no"):
		return tool.InputResponse{Action: tool.InputDecline}
	}
	content := map[string]any{}
	for _, f := range formFields(req.Schema) {
		for {
			fmt.Fprintf(s.out, "    %s: ", f.prompt())
			line, ok := s.readLine()
			if !ok {
				return tool.InputResponse{Action: tool.InputCancel}
			}
			if line == "" && !f.required {
				break
			}
			if v, err := f.parse(line); err == nil {
				content[f.name] = v
				break
			} else {
				fmt.Fprintf(s.out, "    %v\n", err)
			}
		}
	}
	return tool.InputResponse{Action: tool.InputAccept, Content: content}
}

// formField is one property of an elicitation schema: MCP restricts them
// to flat strings, numbers, integers and booleans, strings maybe with an
// enum.
type formField struct {
	name, kind, description string
	enum                    []string
	required                bool
}

func (f formField) prompt() string {
	p := f.name
	if f.description != "" {
		p += " (" + f.description + ")"
	}
	switch {
	case len(f.enum) > 0:
		p += " [" + strings.Join(f.enum, "/") + "]"
	case f.kind == "boolean":
		p += " [y/n]"
	}
	if !f.required {
		p += ", optional"
	}
	return p
}

func (f formField) parse(s string) (any, error) {
	switch f.kind {
	case "integer":
		return strconv.Atoi(s)
	case "number":
		return strconv.ParseFloat(s, 64)
	case "boolean":
		switch strings.ToLower(s) {
		case "y", "yes", "true":
			return true, nil
		case "n", "no", "false":
			return false, nil
		}
		return nil, errors.New("answer y or n")
	}
	if len(f.enum) > 0 && !slices.Contains(f.enum, s) {
		return nil, fmt.Errorf("pick one of %s", strings.Join(f.enum, ", "))
	}
	return s, nil
}

// formFields reads the properties of schema in the order the server wrote
// them.
func formFields(schema json.RawMessage) []formField {
	var s struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(s.Properties)))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out []formField
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return out
		}
		var p struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Title       string   `json:"title"`
			Enum        []string `json:"enum"`
		}
		if dec.Decode(&p) != nil {
			return out
		}
		name := t.(string)
		desc := p.Description
		if desc == "" {
			desc = p.Title
		}
		out = append(out, formField{name: name, kind: p.Type, description: desc, enum: p.Enum, required: slices.Contains(s.Required, name)})
	}
	return out
}

// show prints the model's text as it streams, and a line per tool call and
// per result.
func (s *session) show(ev agent.StreamEvent) {
	switch ev.Type {
	case agent.EventText:
		fmt.Fprint(s.out, ev.Delta)
	case agent.EventToolCall:
		fmt.Fprintf(s.out, "\n  → %s %s\n", ev.Name, ev.Input)
	case agent.EventToolResult:
		fmt.Fprintf(s.out, "  ← %s\n", observation(ev.Output))
	}
}

func (s *session) readLine() (string, bool) {
	if !s.in.Scan() {
		return "", false
	}
	return strings.TrimSpace(s.in.Text()), true
}

// observation is what the model reads of a result, shortened for the
// terminal.
func observation(out string) string {
	r := []rune(string(tool.ProjectModelObservation(json.RawMessage(out))))
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return string(r)
}

func splitNames(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}
