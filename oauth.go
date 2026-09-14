package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// storedAuth is what we persist per server: the client (pre-registered or
// dynamically registered), the endpoints needed to refresh, and the token.
type storedAuth struct {
	ServerURL    string           `json:"server_url"`
	ClientID     string           `json:"client_id"`
	ClientSecret string           `json:"client_secret,omitempty"`
	RedirectURL  string           `json:"redirect_url"`
	AuthURL      string           `json:"auth_url"`
	TokenURL     string           `json:"token_url"`
	AuthStyle    oauth2.AuthStyle `json:"auth_style"`
	Scopes       []string         `json:"scopes,omitempty"`
	Token        *oauth2.Token    `json:"token,omitempty"`
}

func (s *storedAuth) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: s.AuthURL, TokenURL: s.TokenURL, AuthStyle: s.AuthStyle},
		RedirectURL:  s.RedirectURL,
		Scopes:       s.Scopes,
	}
}

func loadStoredAuth(path string) (*storedAuth, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s storedAuth
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}

func saveStoredAuth(path string, s *storedAuth) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// newOAuthHandler builds an authorization code handler for t.URL, caching the
// client and token at path. It is only exercised when the server answers
// 401/403, or when a cached token exists.
func newOAuthHandler(t *target, path string, httpClient *http.Client) (auth.OAuthHandler, error) {
	serverURL := t.URL
	stored, err := loadStoredAuth(path)
	if err != nil {
		return nil, err
	}
	switch {
	case stored != nil && stored.ServerURL != serverURL:
		logf(vLifecycle, "auth: ignoring cached auth in %s (it is for %s)", path, stored.ServerURL)
		stored = nil
	case stored != nil && t.ClientID != "" && stored.ClientID != t.ClientID:
		logf(vLifecycle, "auth: ignoring cached auth in %s (client %s, but configured client id is %s)", path, stored.ClientID, t.ClientID)
		stored = nil
	}

	cfg := &auth.AuthorizationCodeHandlerConfig{
		RequestRefreshToken: true,
		Client:              httpClient,
	}

	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", t.redirectPort())
	switch {
	case t.ClientID != "":
		logf(vLifecycle, "auth: using configured client id %s", t.ClientID)
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: t.ClientID}
		if t.ClientSecret != "" {
			cfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: t.ClientSecret}
		}
	case stored != nil:
		// Reuse the previously registered client. Its redirect URL was registered with it.
		logf(vLifecycle, "auth: using cached client %s from %s", stored.ClientID, path)
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: stored.ClientID}
		if stored.ClientSecret != "" {
			cfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: stored.ClientSecret}
		}
		cfg.RedirectURL = stored.RedirectURL
	default:
		logf(vLifecycle, "auth: no client configured, will use dynamic client registration if needed")
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:              "mcp-inspect",
				RedirectURIs:            []string{cfg.RedirectURL},
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				TokenEndpointAuthMethod: "none",
			},
		}
	}

	cfg.ScopeFilter = func(discovered []string) []string {
		logf(vLifecycle, "auth: discovered scopes: %v", discovered)
		if len(t.Scopes) > 0 {
			logf(vLifecycle, "auth: requesting configured scopes instead: %v", t.Scopes)
			return t.Scopes
		}
		return discovered
	}

	cfg.AuthorizationCodeFetcher = browserFetcher(cfg.RedirectURL)

	cfg.NewTokenSource = func(ctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		logf(vLifecycle, "auth: obtained token %s (client %s, scopes %v, expires %s, refresh token: %t)",
			redact(tok.AccessToken), oc.ClientID, oc.Scopes, formatExpiry(tok), tok.RefreshToken != "")
		s := &storedAuth{
			ServerURL:    serverURL,
			ClientID:     oc.ClientID,
			ClientSecret: oc.ClientSecret,
			RedirectURL:  oc.RedirectURL,
			AuthURL:      oc.Endpoint.AuthURL,
			TokenURL:     oc.Endpoint.TokenURL,
			AuthStyle:    oc.Endpoint.AuthStyle,
			Scopes:       oc.Scopes,
		}
		return newPersistingTokenSource(path, s, oc.TokenSource(ctx, tok), tok)
	}

	if stored != nil && stored.Token != nil {
		if !stored.Token.Valid() && stored.Token.RefreshToken == "" {
			logf(vLifecycle, "auth: cached token expired at %s and has no refresh token, ignoring it", formatExpiry(stored.Token))
		} else {
			logf(vLifecycle, "auth: using cached token %s (expires %s, refresh token: %t)",
				redact(stored.Token.AccessToken), formatExpiry(stored.Token), stored.Token.RefreshToken != "")
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, httpClient)
			ts, err := newPersistingTokenSource(path, stored, stored.oauth2Config().TokenSource(ctx, stored.Token), stored.Token)
			if err != nil {
				return nil, err
			}
			cfg.InitialTokenSource = ts
		}
	}

	handler, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, err
	}
	return &resourceMatchingHandler{OAuthHandler: handler, client: httpClient}, nil
}

// resourceMatchingHandler tolerates a trailing-slash difference between the
// endpoint and the "resource" in the protected resource metadata. The SDK
// compares them as strings and silently ignores the metadata on mismatch.
// Only the metadata URL from the 401's WWW-Authenticate header is checked.
type resourceMatchingHandler struct {
	auth.OAuthHandler
	client *http.Client
}

func (h *resourceMatchingHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	resource, err := h.challengedResource(ctx, resp)
	if err != nil {
		logf(vLifecycle, "auth: %v", err)
	}
	endpoint := req.URL.String()
	if resource != "" && resource != endpoint && strings.TrimSuffix(resource, "/") == strings.TrimSuffix(endpoint, "/") {
		logf(vLifecycle, "auth: metadata resource %q differs from endpoint %q by a trailing slash, authorizing for %q", resource, endpoint, resource)
		u, err := url.Parse(resource)
		if err != nil {
			return err
		}
		req = req.Clone(ctx)
		req.URL = u
	}
	return h.OAuthHandler.Authorize(ctx, req, resp)
}

// challengedResource returns the "resource" from the metadata named in the
// WWW-Authenticate header, or "" if there is none.
func (h *resourceMatchingHandler) challengedResource(ctx context.Context, resp *http.Response) (string, error) {
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	var metadataURL string
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			metadataURL = u
		}
	}
	if metadataURL == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return "", err
	}
	metaResp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer metaResp.Body.Close()
	var prm struct {
		Resource string `json:"resource"`
	}
	if err := json.NewDecoder(metaResp.Body).Decode(&prm); err != nil {
		return "", fmt.Errorf("reading %s: %w", metadataURL, err)
	}
	return prm.Resource, nil
}

func formatExpiry(tok *oauth2.Token) string {
	if tok.Expiry.IsZero() {
		return "never"
	}
	return tok.Expiry.Local().Format(time.RFC3339)
}

// persistingTokenSource saves the token whenever the underlying source returns a new one.
type persistingTokenSource struct {
	path   string
	stored *storedAuth
	base   oauth2.TokenSource

	mu sync.Mutex
}

func newPersistingTokenSource(path string, s *storedAuth, base oauth2.TokenSource, tok *oauth2.Token) (*persistingTokenSource, error) {
	ts := &persistingTokenSource{path: path, stored: s, base: base}
	if s.Token == nil || s.Token.AccessToken != tok.AccessToken {
		s.Token = tok
		if err := saveStoredAuth(path, s); err != nil {
			return nil, err
		}
		logf(vLifecycle, "auth: saved client and token to %s", path)
	}
	return ts, nil
}

func (ts *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := ts.base.Token()
	if err != nil {
		return nil, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if tok.AccessToken != ts.stored.Token.AccessToken {
		logf(vLifecycle, "auth: token refreshed: %s (expires %s)", redact(tok.AccessToken), formatExpiry(tok))
		ts.stored.Token = tok
		if err := saveStoredAuth(ts.path, ts.stored); err != nil {
			return nil, err
		}
	}
	return tok, nil
}

// browserFetcher returns a fetcher that serves the redirect URL locally,
// prints the authorization URL and tries to open it in a browser.
func browserFetcher(redirectURL string) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		u, err := url.Parse(redirectURL)
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", u.Host)
		if err != nil {
			return nil, fmt.Errorf("listening for OAuth callback on %s: %w", u.Host, err)
		}

		type result struct {
			res *auth.AuthorizationResult
			err error
		}
		results := make(chan result, 1)
		mux := http.NewServeMux()
		mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			logf(vLifecycle, "auth: callback received: %s", redactForm(r.URL.RawQuery))
			if e := q.Get("error"); e != "" {
				fmt.Fprintf(w, "Authorization failed: %s %s\n", e, q.Get("error_description"))
				results <- result{err: fmt.Errorf("authorization server returned error: %s %s", e, q.Get("error_description"))}
				return
			}
			fmt.Fprintln(w, "Authorization complete. You can close this window.")
			results <- result{res: &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}}
		})
		srv := &http.Server{Handler: mux}
		go srv.Serve(ln)
		defer srv.Close()

		fmt.Fprintf(os.Stderr, "Open this URL to authorize (waiting for callback on %s):\n\n  %s\n\n", redirectURL, args.URL)
		openBrowser(args.URL)

		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		select {
		case r := <-results:
			return r.res, r.err
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for OAuth callback: %w", ctx.Err())
		}
	}
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		logf(vLifecycle, "auth: could not open browser: %v", err)
		return
	}
	go cmd.Wait()
}
