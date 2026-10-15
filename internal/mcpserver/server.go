// Package mcpserver exposes only the daemon's restricted, confirmation-gated API.
// It never opens a Store, starts an Engine, or loads the daemon configuration.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"quota-session-orchestrator/internal/localapi"
)

var ErrRequest = errors.New("restricted MCP request failed; no automatic retry")

// Backend deliberately has no approval, execution, arbitrary HTTP or hook method.
type Backend interface {
	Status(context.Context) (localapi.Status, error)
	List(context.Context, string, string, int) (localapi.Page, error)
	Requests(context.Context, string, int) (localapi.RequestPage, error)
	RequestAction(context.Context, localapi.ActionInput) (localapi.RequestResponse, error)
}

func New(backend Backend) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "quota-session-orchestrator", Version: "1.0.0"}, nil)
	// SDK input/output schema and unmarshal errors can contain parameter values.
	// Redact at the protocol boundary too, not just in the daemon HTTP client.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				if recover() != nil {
					result, err = nil, ErrRequest
				}
			}()
			if method == "tools/call" {
				b, marshalErr := json.Marshal(req.GetParams())
				if marshalErr != nil || len(b) > 16<<10 {
					return nil, ErrRequest
				}
			}
			result, err = next(ctx, method, req)
			if err != nil {
				return nil, ErrRequest
			}
			if r, ok := result.(*mcp.CallToolResult); ok && r.IsError {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: ErrRequest.Error()}}}, nil
			}
			return result, nil
		}
	})
	registerReads(s, backend)
	registerControls(s, backend)
	return s
}

// RunStdio uses the official SDK transport; stdout contains only MCP frames.
func RunStdio(ctx context.Context, backend Backend) error {
	return Run(ctx, backend, &mcp.StdioTransport{})
}

// Run accepts an isolated SDK transport for protocol tests, never an HTTP listener.
func Run(ctx context.Context, backend Backend, transport mcp.Transport) error {
	if backend == nil || transport == nil {
		return ErrRequest
	}
	if err := New(backend).Run(ctx, transport); err != nil {
		return ErrRequest
	}
	return nil
}

func tool(name, description string, readOnly bool) *mcp.Tool {
	no := false
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: readOnly, IdempotentHint: true, OpenWorldHint: &no, DestructiveHint: &no,
	}}
}
