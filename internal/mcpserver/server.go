package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/domain"
)

const serverVersion = "0.1.0"

type toolSpec struct {
	name        string
	description string
	inputSchema json.RawMessage
}

type emptyInput struct{}

type searchInput struct {
	Query string `json:"query"`
}

type entityInput struct {
	EntityKey string `json:"entity_key"`
}

type relatedContextInput struct {
	EntityKey string `json:"entity_key"`
	Limit     int    `json:"limit"`
}

type noteAddInput struct {
	EntityKey string   `json:"entity_key"`
	Kind      string   `json:"kind"`
	Body      string   `json:"body"`
	Author    string   `json:"author"`
	Topics    []string `json:"topics"`
}

type noteReviseInput struct {
	NoteID string   `json:"note_id"`
	Body   string   `json:"body"`
	Author string   `json:"author"`
	Topics []string `json:"topics"`
}

type contextQueryInput struct {
	EntityKey     string                    `json:"entity_key"`
	BaseContextID string                    `json:"base_context_id"`
	Query         string                    `json:"query"`
	Text          string                    `json:"text"`
	Kinds         []domain.NoteKind         `json:"kinds"`
	States        []domain.NoteBindingState `json:"states"`
	Languages     []string                  `json:"languages"`
	Paths         []string                  `json:"paths"`
	EntityKinds   []domain.EntityKind       `json:"entity_kinds"`
	Topics        []string                  `json:"topics"`
	History       domain.HistoryMode        `json:"history"`
	Limit         int                       `json:"limit"`
}

var (
	searchTool = toolSpec{
		name:        "search",
		description: "Search indexed code entities and committed context notes with lexical evidence.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"symbol, phrase, or note text to search"}},"required":["query"]}`),
	}
	contextGetTool = toolSpec{
		name:        "context_get",
		description: "Read the active context notes bound to one entity key.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"entity_key":{"type":"string"}},"required":["entity_key"]}`),
	}
	contextForChangeTool = toolSpec{
		name:        "context_for_change",
		description: "Assemble bounded context for entity changes since an immutable context snapshot.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"base_context_id":{"type":"string"},"text":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}},"kinds":{"type":"array","items":{"type":"string"}},"states":{"type":"array","items":{"type":"string"}},"history":{"type":"string","enum":["current","all"]},"limit":{"type":"integer","minimum":1,"maximum":100}}}`),
	}
	contextForEntityTool = toolSpec{
		name:        "context_for_entity",
		description: "Assemble bounded current or historical context for one entity with explicit evidence reasons.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"entity_key":{"type":"string"},"text":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}},"kinds":{"type":"array","items":{"type":"string"}},"states":{"type":"array","items":{"type":"string"}},"history":{"type":"string","enum":["current","all"]},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["entity_key"]}`),
	}
	contextQueryTool = toolSpec{
		name:        "context_query",
		description: "Assemble bounded current or historical context from lexical entity and note evidence.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}},"kinds":{"type":"array","items":{"type":"string"}},"states":{"type":"array","items":{"type":"string"}},"history":{"type":"string","enum":["current","all"]},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["query"]}`),
	}
	relatedContextTool = toolSpec{
		name:        "related_context",
		description: "Bounded one-hop structural view: owner type and same-file entities. Never call, import, or impact edges.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"entity_key":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["entity_key"]}`),
	}
	noteAddTool = toolSpec{
		name:        "note_add",
		description: "Draft one evidence-backed pending context note bound to an entity. The note stays pending until a human commits it; the origin is always recorded as agent.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"entity_key":{"type":"string"},"kind":{"type":"string","enum":["intent","decision","constraint","example","warning"]},"body":{"type":"string","description":"evidence-backed knowledge; cite the diff, test, issue, or user statement"},"author":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}}},"required":["entity_key","kind","body"]}`),
	}
	noteReviseTool = toolSpec{
		name:        "note_revise",
		description: "Draft a pending successor revision for an existing note instead of duplicating it.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"note_id":{"type":"string"},"body":{"type":"string"},"author":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}}},"required":["note_id","body"]}`),
	}
	statusTool = toolSpec{
		name:        "status",
		description: "Working-set status: pending notes, coverage, and source state.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	diffTool = toolSpec{
		name:        "diff",
		description: "All pending context changes awaiting an explicit human commit.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
)

func New(service *app.Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "thread-keep", Version: serverVersion}, nil)

	addTool(server, searchTool, func(ctx context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, any, error) {
		hits, err := service.Search(ctx, input.Query)
		return domainResult(map[string]any{"hits": hits}, err)
	})
	addTool(server, contextGetTool, func(ctx context.Context, _ *mcp.CallToolRequest, input entityInput) (*mcp.CallToolResult, any, error) {
		result, err := service.Context(ctx, input.EntityKey)
		return domainResult(result, err)
	})
	addTool(server, contextForChangeTool, func(ctx context.Context, _ *mcp.CallToolRequest, input contextQueryInput) (*mcp.CallToolResult, any, error) {
		result, err := service.AssembleContext(ctx, input.contextQuery(domain.ContextAnchor{Kind: domain.AnchorChange, BaseContextID: input.BaseContextID}))
		return domainResult(result, err)
	})
	addTool(server, contextForEntityTool, func(ctx context.Context, _ *mcp.CallToolRequest, input contextQueryInput) (*mcp.CallToolResult, any, error) {
		result, err := service.AssembleContext(ctx, input.contextQuery(domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: input.EntityKey}))
		return domainResult(result, err)
	})
	addTool(server, contextQueryTool, func(ctx context.Context, _ *mcp.CallToolRequest, input contextQueryInput) (*mcp.CallToolResult, any, error) {
		result, err := service.AssembleContext(ctx, input.contextQuery(domain.ContextAnchor{Kind: domain.AnchorText, Query: input.Query}))
		return domainResult(result, err)
	})
	addTool(server, relatedContextTool, func(ctx context.Context, _ *mcp.CallToolRequest, input relatedContextInput) (*mcp.CallToolResult, any, error) {
		if input.Limit == 0 {
			input.Limit = 20
		}
		related, err := service.RelatedContext(ctx, input.EntityKey, input.Limit)
		return domainResult(map[string]any{"related": related}, err)
	})
	addTool(server, noteAddTool, func(ctx context.Context, _ *mcp.CallToolRequest, input noteAddInput) (*mcp.CallToolResult, any, error) {
		note, err := service.AddNote(ctx, app.AddNoteInput{
			EntityKey: input.EntityKey,
			Kind:      input.Kind,
			Body:      input.Body,
			Author:    defaultAuthor(input.Author),
			Origin:    "agent",
			Topics:    input.Topics,
		})
		return domainResult(note, err)
	})
	addTool(server, noteReviseTool, func(ctx context.Context, _ *mcp.CallToolRequest, input noteReviseInput) (*mcp.CallToolResult, any, error) {
		note, err := service.ReviseNote(ctx, app.ReviseNoteInput{
			NoteID: input.NoteID,
			Body:   input.Body,
			Author: defaultAuthor(input.Author),
			Origin: "agent",
			Topics: input.Topics,
		})
		return domainResult(note, err)
	})
	addTool(server, statusTool, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		status, err := service.Status(ctx)
		return domainResult(status, err)
	})
	addTool(server, diffTool, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		notes, err := service.Diff(ctx)
		return domainResult(map[string]any{"pending": notes}, err)
	})

	return server
}

func addTool[Input any](server *mcp.Server, spec toolSpec, handler mcp.ToolHandlerFor[Input, any]) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        spec.name,
		Description: spec.description,
		InputSchema: spec.inputSchema,
	}, handler)
}

func (i contextQueryInput) contextQuery(anchor domain.ContextAnchor) domain.ContextQuery {
	return domain.ContextQuery{
		Anchor:      anchor,
		Text:        i.Text,
		Kinds:       i.Kinds,
		States:      i.States,
		Languages:   i.Languages,
		Paths:       i.Paths,
		EntityKinds: i.EntityKinds,
		Topics:      i.Topics,
		History:     i.History,
		Limit:       i.Limit,
	}
}

func defaultAuthor(author string) string {
	if author == "" {
		return "agent"
	}
	return author
}

func domainResult(payload any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		code := domain.CodeOf(err)
		if code == "" {
			code = domain.CodeLocalStorage
		}
		return textResult(map[string]any{"code": code, "message": err.Error()}, true), nil, nil
	}
	return textResult(payload, false), nil, nil
}

func textResult(payload any, isError bool) *mcp.CallToolResult {
	text, err := json.Marshal(payload)
	if err != nil {
		text, _ = json.Marshal(map[string]any{"code": domain.CodeLocalStorage, "message": "serialize tool result"})
		isError = true
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
		IsError: isError,
	}
}
