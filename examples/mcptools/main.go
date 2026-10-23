// Command mcptools connects to an MCP server, registers its tools into a
// dialogram tool.Registry and prints what the model would see: the
// registered names and the Anthropic tool spec with each server's input
// schema passed through.
//
// A stdio server is given as the command after the flags:
//
//	go run ./examples/mcptools -- npx -y @modelcontextprotocol/server-filesystem /tmp
//
// A streamable HTTP server is given with -url:
//
//	go run ./examples/mcptools -url http://localhost:3001/mcp -header "Authorization: Bearer …"
//
// Add -call NAME [-args JSON] to invoke one registered tool after listing
// and print the mapped result, e.g.
//
//	go run ./examples/mcptools -call filesystem__list_directory -args '{"path":"/tmp"}' -- npx -y @modelcontextprotocol/server-filesystem /tmp
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Cyprinus12138/dialogram/tool"
	"github.com/Cyprinus12138/dialogram/tool/mcp"
)

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

func main() {
	var (
		name    = flag.String("name", "", "server label used as the tool-name prefix (default: the server's advertised name)")
		timeout = flag.Duration("timeout", 60*time.Second, "per-call timeout")
		url     = flag.String("url", "", "streamable HTTP endpoint (instead of a stdio command)")
		call    = flag.String("call", "", "registered tool name to invoke after listing")
		args    = flag.String("args", "{}", "JSON arguments for -call")
		headers headerFlag
	)
	flag.Var(&headers, "header", "extra HTTP header as \"Key: Value\" (repeatable; -url only)")
	flag.Parse()

	var transport mcp.Transport
	switch {
	case *url != "":
		transport = mcp.StreamableHTTP{URL: *url, Header: headers.h}
	case flag.NArg() > 0:
		transport = mcp.Stdio{Command: flag.Arg(0), Args: flag.Args()[1:]}
	default:
		fmt.Fprintln(os.Stderr, "usage: mcptools [flags] -- <command> [args...]   or   mcptools -url <endpoint>")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := mcp.Connect(ctx, transport, mcp.Options{Name: *name, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer client.Close()

	reg := tool.NewRegistry()
	names, err := mcp.Register(ctx, reg, client, mcp.RegisterOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}

	fmt.Printf("server %q (%d tools)\n", client.Name(), len(names))
	if ins := client.Instructions(); ins != "" {
		fmt.Printf("instructions: %s\n", ins)
	}
	for _, n := range names {
		fmt.Println("  ", n)
	}

	spec, _ := json.MarshalIndent(reg.AnthropicSpec(), "", "  ")
	fmt.Println(string(spec))

	if *call != "" {
		out, err := reg.Call(ctx, *call, *args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "call:", err)
			os.Exit(1)
		}
		fmt.Printf("\n%s → %s\n", *call, out)
	}
}
