// Package testservers provides MCP servers for testing mcp-inspect, both from
// Go tests and by hand (see testdata/fakeauth and testdata/schemaserver).
package testservers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// AuthServer is a fake OAuth authorization server in front of a protected MCP
// endpoint. It implements just enough for the MCP authorization flow:
// protected resource metadata, authorization server metadata, dynamic client
// registration, an authorization endpoint that approves immediately, and a
// token endpoint with refresh tokens.
//
// Routes, relative to the server's base URL:
//
//	/mcp                                          protected MCP endpoint (NewSchemaServer)
//	/.well-known/oauth-protected-resource/mcp     protected resource metadata
//	/.well-known/oauth-authorization-server/as    authorization server metadata (issuer <base>/as)
//	/as/register, /as/authorize, /as/token
//
// The issuer lives under /as, so a client that ignores the resource metadata
// and falls back to legacy discovery at the root fails.
type AuthServer struct {
	// ResourceSuffix is appended to the resource in the protected resource
	// metadata, e.g. "/" for a trailing-slash mismatch. Set before serving.
	ResourceSuffix string
	// DenyAuthorization makes /as/authorize redirect back with access_denied.
	// Set before serving.
	DenyAuthorization bool
	// TokenLifetime of issued access tokens; defaults to one hour.
	TokenLifetime time.Duration
	// Logf, if set, receives a line per event.
	Logf func(format string, args ...any)

	mu            sync.Mutex
	counter       int
	accessTokens  map[string]time.Time
	refreshTokens map[string]bool
	events        []string
}

func NewAuthServer() *AuthServer {
	return &AuthServer{accessTokens: map[string]time.Time{}, refreshTokens: map[string]bool{}}
}

// Events returns what happened so far, in order: "no-token" (MCP request
// without a token), "access-rejected" (MCP request with an invalid token),
// "register", "authorize", "token:authorization_code", "token:refresh_token",
// "refresh-rejected".
func (s *AuthServer) Events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// ResetEvents clears the event list.
func (s *AuthServer) ResetEvents() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
}

// Forget drops all issued access and refresh tokens, as after a restart.
func (s *AuthServer) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessTokens = map[string]time.Time{}
	s.refreshTokens = map[string]bool{}
}

// IssueToken returns a new valid access token without going through OAuth.
func (s *AuthServer) IssueToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueLocked()
}

func (s *AuthServer) event(e string, detail string) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	if s.Logf != nil {
		s.Logf("%s %s", e, detail)
	}
}

func (s *AuthServer) nextLocked(prefix string) string {
	s.counter++
	return fmt.Sprintf("%s-%d-0123456789", prefix, s.counter)
}

func (s *AuthServer) issueLocked() string {
	lifetime := s.TokenLifetime
	if lifetime == 0 {
		lifetime = time.Hour
	}
	at := s.nextLocked("access")
	s.accessTokens[at] = time.Now().Add(lifetime)
	return at
}

// Handler serves all routes. URLs in metadata are derived from the request's host.
func (s *AuthServer) Handler() http.Handler {
	mcpHandler := NewStreamableHandler(NewSchemaServer())
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		base := baseURL(r)
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:             base + "/mcp" + s.ResourceSuffix,
			AuthorizationServers: []string{base + "/as"},
			ScopesSupported:      []string{"read"},
		}).ServeHTTP(w, r)
	})

	mux.HandleFunc("/.well-known/oauth-authorization-server/as", func(w http.ResponseWriter, r *http.Request) {
		issuer := baseURL(r) + "/as"
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"registration_endpoint":                 issuer + "/register",
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"scopes_supported":                      []string{"read", "offline_access"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})

	mux.HandleFunc("POST /as/register", func(w http.ResponseWriter, r *http.Request) {
		var metadata map[string]any
		if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
			return
		}
		s.mu.Lock()
		metadata["client_id"] = s.nextLocked("client")
		s.mu.Unlock()
		s.event("register", metadata["client_id"].(string))
		writeJSON(w, http.StatusCreated, metadata)
	})

	mux.HandleFunc("/as/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.event("authorize", "client="+q.Get("client_id"))
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		values := url.Values{"state": {q.Get("state")}}
		if s.DenyAuthorization {
			values.Set("error", "access_denied")
		} else {
			s.mu.Lock()
			values.Set("code", s.nextLocked("code"))
			s.mu.Unlock()
		}
		redirect.RawQuery = values.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})

	mux.HandleFunc("POST /as/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
			return
		}
		grant := r.Form.Get("grant_type")
		s.mu.Lock()
		if grant == "refresh_token" {
			rt := r.Form.Get("refresh_token")
			if !s.refreshTokens[rt] {
				s.mu.Unlock()
				s.event("refresh-rejected", rt)
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
				return
			}
			delete(s.refreshTokens, rt)
		}
		at := s.issueLocked()
		rt := s.nextLocked("refresh")
		s.refreshTokens[rt] = true
		lifetime := s.accessTokens[at]
		s.mu.Unlock()
		s.event("token:"+grant, at)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  at,
			"token_type":    "Bearer",
			"expires_in":    int(time.Until(lifetime).Seconds()),
			"refresh_token": rt,
			"scope":         "read",
		})
	})

	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		s.mu.Lock()
		expiry, ok := s.accessTokens[token]
		s.mu.Unlock()
		if !ok || time.Now().After(expiry) {
			s.event("access-rejected", token)
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Scopes: []string{"read"}, Expiration: expiry}, nil
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			// RequireBearerToken rejects these without calling verify.
			s.event("no-token", r.Method)
		}
		auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
			Scopes:              []string{"read"},
			ResourceMetadataURL: baseURL(r) + "/.well-known/oauth-protected-resource/mcp",
		})(mcpHandler).ServeHTTP(w, r)
	})

	return mux
}

func baseURL(r *http.Request) string {
	return "http://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
