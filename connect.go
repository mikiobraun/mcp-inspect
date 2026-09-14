package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect starts a session with the server described by t. authPath is where
// OAuth clients and tokens are cached; it is only used for URL targets.
// Raw results of the session's requests are recorded in raw.
func connect(ctx context.Context, t *target, authPath string, raw *rawResults) (*mcp.ClientSession, error) {
	var transport mcp.Transport
	var stderr *tailBuffer
	if t.URL != "" {
		var err error
		transport, err = httpTransport(t, authPath)
		if err != nil {
			return nil, err
		}
	} else {
		logf(vLifecycle, "transport: stdio, command %q", t.Command)
		cmd := exec.Command(t.Command[0], t.Command[1:]...)
		stderr = &tailBuffer{max: 8192}
		cmd.Stderr = &serverStderr{tail: stderr}
		transport = &mcp.CommandTransport{Command: cmd}
	}

	transport = &loggingTransport{next: transport, raw: raw}

	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-inspect", Version: "0.1.0"}, &mcp.ClientOptions{
		// Advertise no capabilities: the SDK's default announces roots, which we don't serve.
		Capabilities: &mcp.ClientCapabilities{},
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			p := req.Params
			logf(vLifecycle, "server log [%s] %s: %v", p.Level, p.Logger, p.Data)
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			p := req.Params
			logf(vLifecycle, "server progress %v/%v %s", p.Progress, p.Total, p.Message)
		},
	})
	logf(vLifecycle, "initialize: connecting")
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		if stderr != nil && verbosity < vLifecycle && stderr.Len() > 0 {
			fmt.Fprintf(os.Stderr, "server stderr:\n%s\n", stderr.String())
		}
		return nil, fmt.Errorf("connecting: %w", err)
	}
	res := session.InitializeResult()
	logf(vLifecycle, "initialize: done, server %s %s, protocol %s", res.ServerInfo.Name, res.ServerInfo.Version, res.ProtocolVersion)
	return session, nil
}

func httpTransport(t *target, authPath string) (mcp.Transport, error) {
	endpoint := t.URL
	headers := http.Header{}
	for _, h := range t.Headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("invalid header %q, expected 'Name: value'", h)
		}
		headers.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}

	logged := &loggingRoundTripper{next: http.DefaultTransport}

	var handler auth.OAuthHandler
	switch {
	case t.Bearer != "" && t.BearerCmd != "":
		return nil, errors.New("bearer and bearer-cmd are mutually exclusive")
	case (t.Bearer != "" || t.BearerCmd != "") && headers.Get("Authorization") != "":
		return nil, errors.New("bearer* and an Authorization header are mutually exclusive")
	case t.Bearer != "":
		logf(vLifecycle, "auth: static bearer token %s", redact(t.Bearer))
		handler = &staticBearer{token: t.Bearer}
	case t.BearerCmd != "":
		logf(vLifecycle, "auth: bearer token from command")
		handler = &cmdBearer{command: t.BearerCmd}
	case headers.Get("Authorization") != "":
		logf(vLifecycle, "auth: Authorization header configured, OAuth disabled")
	default:
		logf(vLifecycle, "auth: OAuth on demand (when the server answers 401), cache %s", authPath)
		var err error
		handler, err = newOAuthHandler(t, authPath, &http.Client{Transport: logged})
		if err != nil {
			return nil, err
		}
	}

	httpClient := &http.Client{Transport: &authRoundTripper{next: logged, headers: headers, handler: handler}}
	if t.SSE {
		logf(vLifecycle, "transport: legacy SSE, endpoint %s", endpoint)
		return &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: httpClient}, nil
	}
	logf(vLifecycle, "transport: streamable HTTP, endpoint %s", endpoint)
	// The standalone GET stream only matters for server-initiated messages,
	// which a one-shot inspection doesn't wait for.
	return &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil
}

// loggingTransport logs JSON-RPC messages at -vv and records raw results.
type loggingTransport struct {
	next mcp.Transport
	raw  *rawResults
}

func (t *loggingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.next.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &loggingConn{Connection: conn, raw: t.raw}, nil
}

type loggingConn struct {
	mcp.Connection
	raw *rawResults
}

func (c *loggingConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	logMessage("←", msg, err)
	if resp, ok := msg.(*jsonrpc.Response); ok && err == nil {
		c.raw.response(resp)
	}
	return msg, err
}

func (c *loggingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.ID.IsValid() {
		c.raw.request(req)
	}
	err := c.Connection.Write(ctx, msg)
	logMessage("→", msg, err)
	return err
}

// rawResults keeps the raw JSON results of requests, by method, in the order
// they arrived. The SDK decodes results into Go maps, which loses details
// like the order of properties in tool schemas.
type rawResults struct {
	mu      sync.Mutex
	pending map[any]string // request id -> method
	results map[string][]json.RawMessage
}

func newRawResults() *rawResults {
	return &rawResults{pending: map[any]string{}, results: map[string][]json.RawMessage{}}
}

func (r *rawResults) request(req *jsonrpc.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[req.ID.Raw()] = req.Method
}

func (r *rawResults) response(resp *jsonrpc.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	method, ok := r.pending[resp.ID.Raw()]
	if !ok || resp.Error != nil {
		return
	}
	delete(r.pending, resp.ID.Raw())
	r.results[method] = append(r.results[method], resp.Result)
}

// get returns the raw results for method, e.g. one per page of tools/list.
func (r *rawResults) get(method string) []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.results[method]
}

func logMessage(dir string, msg jsonrpc.Message, err error) {
	if verbosity < vJSONRPC {
		return
	}
	if err != nil {
		logf(vJSONRPC, "%s error: %v", dir, err)
		return
	}
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		logf(vJSONRPC, "%s <unencodable message: %v>", dir, err)
		return
	}
	logf(vJSONRPC, "%s %s", dir, data)
}

// serverStderr forwards a stdio server's stderr at -v and always keeps the tail,
// so it can be shown when the connection fails.
type serverStderr struct {
	tail *tailBuffer
}

func (s *serverStderr) Write(p []byte) (int, error) {
	s.tail.Write(p)
	logf(vLifecycle, "server stderr: %s", p)
	return len(p), nil
}

type tailBuffer struct {
	max int
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if over := b.buf.Len() - b.max; over > 0 {
		b.buf.Next(over)
	}
	return len(p), nil
}

func (b *tailBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
