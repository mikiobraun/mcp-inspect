package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// authRoundTripper adds static headers and the Authorization header from an
// auth.OAuthHandler. On 401/403 it calls the handler's Authorize once and
// retries the request. This follows the OAuthHandler contract, and works the
// same for streamable HTTP and legacy SSE (which has no OAuthHandler support).
//
// A failed authorization is not attempted again. The SDK keeps sending after a
// failure (a fallback initialize, a cancellation notice on a context that
// Ctrl+C doesn't cancel), and each would otherwise start a new OAuth flow.
type authRoundTripper struct {
	next    http.RoundTripper
	headers http.Header
	handler auth.OAuthHandler // nil: no authorization

	mu      sync.Mutex // serializes Authorize calls, guards authErr
	authErr error      // first authorization failure
}

func (t *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body so the request can be retried after authorization.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	resp, err := t.send(req, body)
	if err != nil || t.handler == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}

	t.mu.Lock()
	if t.authErr != nil {
		t.mu.Unlock()
		resp.Body.Close()
		logf(vLifecycle, "auth: %s %s returned %s, not retrying after failed authorization", req.Method, req.URL, resp.Status)
		return nil, t.authErr
	}
	logf(vLifecycle, "auth: %s %s returned %s, running authorization", req.Method, req.URL, resp.Status)
	err = t.handler.Authorize(req.Context(), req, resp)
	if err != nil {
		t.authErr = fmt.Errorf("authorization failed: %w", err)
		err = t.authErr
	}
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	logf(vLifecycle, "auth: authorization done, retrying %s %s", req.Method, req.URL)
	return t.send(req, body)
}

func (t *authRoundTripper) send(orig *http.Request, body []byte) (*http.Response, error) {
	req := orig.Clone(orig.Context())
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.ContentLength = int64(len(body))
	}
	for k, vs := range t.headers {
		req.Header[k] = vs
	}
	if t.handler != nil {
		ts, err := t.handler.TokenSource(req.Context())
		if err != nil {
			return nil, err
		}
		if ts != nil {
			tok, err := ts.Token()
			var retrieveErr *oauth2.RetrieveError
			switch {
			case errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant":
				// Refresh token no longer valid: send without token, the 401 triggers Authorize.
				logf(vLifecycle, "auth: token refresh rejected (invalid_grant), sending without token")
			case err != nil:
				return nil, fmt.Errorf("getting token: %w", err)
			default:
				req.Header.Set("Authorization", tok.Type()+" "+tok.AccessToken)
			}
		}
	}
	return t.next.RoundTrip(req)
}

// redirectUpgradingRoundTripper rewrites a redirect from an https request to
// http on the same host into an https redirect. Servers behind a TLS-terminating
// proxy often build redirects with http; the SDK rightly refuses to follow
// those. Rewriting the Location header keeps the SDK's redirect checks in place.
type redirectUpgradingRoundTripper struct {
	next http.RoundTripper
}

func (t *redirectUpgradingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || req.URL.Scheme != "https" || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return resp, err
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme != "http" || loc.Hostname() != req.URL.Hostname() {
		return resp, nil
	}
	if port := loc.Port(); port != "" && port != "80" && port != req.URL.Port() {
		return resp, nil
	}
	upgraded := *loc
	upgraded.Scheme = "https"
	upgraded.Host = req.URL.Host
	logf(vLifecycle, "auth: %s %s redirects to %s, following %s instead (--upgrade-redirects)", req.Method, req.URL, loc, &upgraded)
	resp.Header.Set("Location", upgraded.String())
	return resp, nil
}

// staticBearer uses a fixed token. A 401/403 is a hard error.
type staticBearer struct {
	token string
}

func (b *staticBearer) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: b.token, TokenType: "Bearer"}), nil
}

func (b *staticBearer) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	resp.Body.Close()
	return fmt.Errorf("server rejected the bearer token (%s)", resp.Status)
}

// cmdBearer gets its token from a shell command. The command runs on first
// use and again when the server rejects the current token.
type cmdBearer struct {
	command string

	mu    sync.Mutex
	token string
}

func (b *cmdBearer) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token == "" {
		if err := b.fetch(ctx); err != nil {
			return nil, err
		}
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: b.token, TokenType: "Bearer"}), nil
}

func (b *cmdBearer) Authorize(ctx context.Context, _ *http.Request, resp *http.Response) error {
	resp.Body.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	logf(vLifecycle, "auth: token rejected, re-running bearer command")
	return b.fetch(ctx)
}

func (b *cmdBearer) fetch(ctx context.Context) error {
	logf(vLifecycle, "auth: running bearer command: %s", b.command)
	cmd := exec.CommandContext(ctx, "sh", "-c", b.command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("bearer command failed: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return errors.New("bearer command printed no token")
	}
	logf(vLifecycle, "auth: bearer command returned token %s", redact(token))
	b.token = token
	return nil
}
