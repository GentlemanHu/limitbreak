package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/mcpserver"
)

var errMCPArguments = errors.New("MCP requires --profile <independent 0600 scoped profile>; only stdio is supported")

func runMCP(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcpCommand(ctx, args, &mcp.StdioTransport{}, os.Stderr)
}

// This branch runs before the daemon's --config parser. There is no default
// profile, full-token fallback, environment secret lookup or daemon bootstrap.
func mcpCommand(ctx context.Context, args []string, transport mcp.Transport, stderr io.Writer) error {
	f := flag.NewFlagSet("mcp", flag.ContinueOnError)
	f.SetOutput(io.Discard) // flag errors include user-provided values and paths.
	profile := f.String("profile", "", "independent restricted access profile")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := io.WriteString(stderr, "quota-session-orchestrator mcp --profile <independent 0600 scoped profile>\nTransport: stdio; requires an existing daemon. Controls only request user confirmation.\n")
			if err != nil {
				return errMCPArguments
			}
			return nil
		}
		return errMCPArguments
	}
	if *profile == "" || f.NArg() != 0 {
		return errMCPArguments
	}
	p, err := localapi.LoadProfile(*profile)
	if err != nil {
		return localapi.ErrProfile
	}
	client, err := localapi.New(p)
	if err != nil {
		return localapi.ErrProfile
	}
	defer client.Close()
	return mcpserver.Run(ctx, client, transport)
}
