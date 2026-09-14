package testservers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SearchInputSchema exercises ordered properties, $defs/$ref, anyOf, enums,
// defaults, constraints, nullable types and a boolean schema.
const SearchInputSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "what to look for", "minLength": 1, "maxLength": 200},
    "mode": {"type": "string", "enum": ["fast", "thorough"], "default": "fast", "description": "search strategy"},
    "limit": {"type": ["integer", "null"], "minimum": 1, "maximum": 1000, "default": 100},
    "filter": {"$ref": "#/$defs/Filter", "description": "optional filter"},
    "target": {"anyOf": [{"type": "string", "format": "uri"}, {"type": "integer"}], "description": "URL or numeric id"},
    "tags": {"type": "array", "items": {"type": "string", "enum": ["a", "b", "c"]}, "uniqueItems": true},
    "anything": true
  },
  "required": ["query", "target"],
  "$defs": {
    "Filter": {
      "type": "object",
      "description": "restricts results",
      "properties": {
        "path": {"type": "string", "description": "folder prefix"},
        "since": {"type": "string", "format": "date-time"}
      },
      "required": ["path"]
    }
  }
}`

// SearchOutputSchema has an array of $ref items.
const SearchOutputSchema = `{
  "type": "object",
  "properties": {"hits": {"type": "array", "items": {"$ref": "#/$defs/Hit"}}},
  "$defs": {"Hit": {"type": "object", "properties": {"path": {"type": "string"}, "score": {"type": "number"}}}}
}`

// NewSchemaServer returns a server named "schemas" with these tools:
//
//	search  feature-rich input and output schemas, annotations; returns no hits
//	echo    returns its arguments as structured content
//	fail    always returns a tool error (isError)
//	noargs  empty object schema
func NewSchemaServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "schemas", Version: "1.0.0"},
		&mcp.ServerOptions{Instructions: "A server for testing schema rendering."})

	server.AddTool(&mcp.Tool{
		Name:         "search",
		Title:        "Search things",
		Description:  "Search for things.\n\nSupports two modes.",
		InputSchema:  json.RawMessage(SearchInputSchema),
		OutputSchema: json.RawMessage(SearchOutputSchema),
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: `{"hits":[]}`}},
			StructuredContent: map[string]any{"hits": []any{}},
		}, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Return the arguments.",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}},
			StructuredContent: req.Params.Arguments,
		}, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "fail",
		Description: "Always fail.",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "this tool always fails"}},
			IsError: true,
		}, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "noargs",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	return server
}

// NewStreamableHandler serves server over streamable HTTP.
func NewStreamableHandler(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

// NewSSEHandler serves server over the legacy SSE transport.
func NewSSEHandler(server *mcp.Server) http.Handler {
	return mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
}
