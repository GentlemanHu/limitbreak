package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/store"
)

type EmptyInput struct{}

type PageInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from the previous page; no private identifier or path"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Page size from 1 to 100; omitted or zero selects 50"`
}

// RequestSummary excludes grant identity, action parameters and all free text.
// The request ID is caller-issued; session IDs are daemon-issued scoped handles.
type RequestSummary struct {
	ID                 string               `json:"id"`
	Digest             string               `json:"digest"`
	State              string               `json:"state"`
	Operation          string               `json:"operation"`
	SessionID          string               `json:"session_id"`
	ExpectedGeneration int64                `json:"expected_generation"`
	PolicyDigest       string               `json:"policy_digest,omitempty"`
	CreatedAt          time.Time            `json:"created_at"`
	ExpiresAt          time.Time            `json:"expires_at"`
	ApprovedAt         *time.Time           `json:"approved_at,omitempty"`
	ExecutingAt        *time.Time           `json:"executing_at,omitempty"`
	CompletedAt        *time.Time           `json:"completed_at,omitempty"`
	Result             *store.ControlResult `json:"result,omitempty"`
}

type RequestsOutput struct {
	Requests   []RequestSummary `json:"requests"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type RequestOutput struct {
	Request              RequestSummary `json:"request"`
	RequiresConfirmation bool           `json:"requires_confirmation"`
}

func summary(r localapi.RequestView) RequestSummary {
	return RequestSummary{ID: r.ID, Digest: r.Digest, State: r.State, Operation: r.Action.Operation,
		SessionID: r.Action.SessionID, ExpectedGeneration: r.Action.ExpectedGeneration, PolicyDigest: r.Action.PolicyDigest,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ApprovedAt: r.ApprovedAt, ExecutingAt: r.ExecutingAt,
		CompletedAt: r.CompletedAt, Result: r.Result}
}

func registerReads(s *mcp.Server, b Backend) {
	mcp.AddTool(s, tool("status", "Read bounded daemon status and policy digest; never authorizes execution.", true),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, localapi.Status, error) {
			out, err := b.Status(ctx)
			if err != nil || localapi.ValidateStatus(out) != nil {
				return nil, localapi.Status{}, ErrRequest
			}
			return nil, out, nil
		})
	for _, collection := range []string{"resources", "sources", "sessions", "queue", "adapters", "diagnostics"} {
		mcp.AddTool(s, tool(collection, "Read a bounded page of scoped "+collection+"; no prompts, body, email, cwd, titles or error text.", true),
			func(ctx context.Context, _ *mcp.CallToolRequest, in PageInput) (*mcp.CallToolResult, localapi.Page, error) {
				if !validPage(in) {
					return nil, localapi.Page{}, ErrRequest
				}
				out, err := b.List(ctx, collection, in.Cursor, in.Limit)
				if err != nil || localapi.ValidatePage(out, collection, pageLimit(in.Limit)) != nil {
					return nil, localapi.Page{}, ErrRequest
				}
				return nil, out, nil
			})
	}
	mcp.AddTool(s, tool("requests", "Read bounded confirmation request states; no approval or execution is possible.", true),
		func(ctx context.Context, _ *mcp.CallToolRequest, in PageInput) (*mcp.CallToolResult, RequestsOutput, error) {
			if !validPage(in) {
				return nil, RequestsOutput{}, ErrRequest
			}
			page, err := b.Requests(ctx, in.Cursor, in.Limit)
			if err != nil || localapi.ValidateRequests(page, pageLimit(in.Limit)) != nil {
				return nil, RequestsOutput{}, ErrRequest
			}
			out := RequestsOutput{Requests: []RequestSummary{}, NextCursor: page.NextCursor}
			for _, r := range page.Requests {
				out.Requests = append(out.Requests, summary(r))
			}
			return nil, out, nil
		})
}

func pageLimit(n int) int {
	if n == 0 {
		return 50
	}
	return n
}
func validPage(in PageInput) bool {
	return pageLimit(in.Limit) >= 1 && pageLimit(in.Limit) <= 100 && len(in.Cursor) <= 128
}
