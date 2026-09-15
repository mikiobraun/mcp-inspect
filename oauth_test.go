package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikiobraun/mcp-inspect/internal/testservers"
)

// oauthSetup starts the fake authorization server, isolates the config dir,
// and replaces the browser with an HTTP client that follows the redirects.
type oauthSetup struct {
	auth     *testservers.AuthServer
	url      string // protected MCP endpoint
	port     string // OAuth callback port
	authPath string // cache for the ad-hoc URL target
}

func newOAuthSetup(t *testing.T, configure func(*testservers.AuthServer)) *oauthSetup {
	return newOAuthSetupWith(t, configure, false)
}

// newTLSOAuthSetup serves over https; the test certificate is trusted by
// replacing http.DefaultTransport for the duration of the test.
func newTLSOAuthSetup(t *testing.T, configure func(*testservers.AuthServer)) *oauthSetup {
	return newOAuthSetupWith(t, configure, true)
}

func newOAuthSetupWith(t *testing.T, configure func(*testservers.AuthServer), useTLS bool) *oauthSetup {
	isolateConfig(t)
	s := testservers.NewAuthServer()
	if configure != nil {
		configure(s)
	}
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(s.Handler())
		original := http.DefaultTransport
		http.DefaultTransport = srv.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = original })
	} else {
		srv = httptest.NewServer(s.Handler())
	}
	t.Cleanup(srv.Close)

	original := openBrowser
	openBrowser = func(u string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Errorf("following authorization URL: %v", err)
			return
		}
		resp.Body.Close()
	}
	t.Cleanup(func() { openBrowser = original })

	endpoint := srv.URL + "/mcp"
	authPath, err := urlAuthPath(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return &oauthSetup{auth: s, url: endpoint, port: freePort(t), authPath: authPath}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// tools runs "tools" against the endpoint and returns the server events it caused.
func (o *oauthSetup) tools(t *testing.T, extra ...string) ([]string, cliResult) {
	t.Helper()
	o.auth.ResetEvents()
	res := runCLI(t, "", append([]string{"tools", "--redirect-port", o.port, o.url}, extra...)...)
	return o.auth.Events(), res
}

func (o *oauthSetup) expireCachedToken(t *testing.T) {
	t.Helper()
	stored, err := loadStoredAuth(o.authPath)
	if err != nil || stored == nil {
		t.Fatalf("loading %s: %v", o.authPath, err)
	}
	stored.Token.Expiry = time.Now().Add(-time.Hour)
	if err := saveStoredAuth(o.authPath, stored); err != nil {
		t.Fatal(err)
	}
}

func requireSuccess(t *testing.T, res cliResult) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("mcp-inspect: %v\nstdout:\n%s\nstderr:\n%s", res.err, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "4 tools") {
		t.Fatalf("unexpected output:\n%s", res.stdout)
	}
}

func requireEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("server events: got %q, want %q", got, want)
	}
}

func TestOAuthFreshFlowThenCachedToken(t *testing.T) {
	o := newOAuthSetup(t, nil)

	events, res := o.tools(t)
	requireSuccess(t, res)
	requireEvents(t, events, "no-token", "register", "authorize", "token:authorization_code")

	info, err := os.Stat(o.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s has mode %v, want 0600", o.authPath, info.Mode().Perm())
	}

	events, res = o.tools(t)
	requireSuccess(t, res)
	requireEvents(t, events) // cached token, no auth traffic
}

func TestOAuthRefreshesExpiredToken(t *testing.T) {
	o := newOAuthSetup(t, nil)
	_, res := o.tools(t)
	requireSuccess(t, res)
	before, _ := loadStoredAuth(o.authPath)

	o.expireCachedToken(t)
	events, res := o.tools(t)
	requireSuccess(t, res)
	requireEvents(t, events, "token:refresh_token")

	after, _ := loadStoredAuth(o.authPath)
	if after.Token.AccessToken == before.Token.AccessToken {
		t.Error("refreshed token was not saved")
	}
}

func TestOAuthRejectedRefreshReauthorizesWithCachedClient(t *testing.T) {
	o := newOAuthSetup(t, nil)
	_, res := o.tools(t)
	requireSuccess(t, res)

	o.auth.Forget()
	o.expireCachedToken(t)
	events, res := o.tools(t)
	requireSuccess(t, res)
	// The request goes out without a token after the failed refresh. No
	// "register": the cached client is reused.
	requireEvents(t, events, "refresh-rejected", "no-token", "authorize", "token:authorization_code")
}

func TestOAuthRejectedAccessTokenReauthorizesWithCachedClient(t *testing.T) {
	o := newOAuthSetup(t, nil)
	_, res := o.tools(t)
	requireSuccess(t, res)

	o.auth.Forget()
	events, res := o.tools(t)
	requireSuccess(t, res)
	requireEvents(t, events, "access-rejected", "authorize", "token:authorization_code")
}

func TestOAuthToleratesTrailingSlashResource(t *testing.T) {
	o := newOAuthSetup(t, func(s *testservers.AuthServer) { s.ResourceSuffix = "/" })
	events, res := o.tools(t)
	requireSuccess(t, res)
	requireEvents(t, events, "no-token", "register", "authorize", "token:authorization_code")
}

func TestOAuthRedirectToHTTP(t *testing.T) {
	slashRedirect := func(s *testservers.AuthServer) { s.IssuerSlashRedirect = true }

	t.Run("refused by default", func(t *testing.T) {
		o := newTLSOAuthSetup(t, slashRedirect)
		events, res := o.tools(t)
		if res.err == nil || !strings.Contains(res.err.Error(), "security downgrade") || !strings.Contains(res.err.Error(), "--upgrade-redirects") {
			t.Fatalf("want downgrade error with a hint, got %v", res.err)
		}
		// The second "no-token" is the SDK's fallback initialize, which is not
		// authorized again.
		requireEvents(t, events, "no-token", "slash-redirect", "no-token")
	})

	// A full flow isn't possible here: after the upgrade, the SDK refuses to
	// follow any redirect into a loopback address, and the test server is on
	// 127.0.0.1. Reaching that check shows the redirect passed as https.
	t.Run("upgraded with --upgrade-redirects", func(t *testing.T) {
		o := newTLSOAuthSetup(t, slashRedirect)
		_, res := o.tools(t, "--upgrade-redirects")
		if res.err == nil || strings.Contains(res.err.Error(), "security downgrade") || !strings.Contains(res.err.Error(), `redirect into loopback address "127.0.0.1"`) {
			t.Fatalf("want the https redirect to reach the loopback check, got %v", res.err)
		}
		if !strings.Contains(res.err.Error(), "https://127.0.0.1:") {
			t.Errorf("error should name the upgraded https URL: %v", res.err)
		}
	})
}

func TestRedirectUpgradingRoundTripper(t *testing.T) {
	for _, tc := range []struct {
		request, location, want string
	}{
		{"https://a.example/x/", "http://a.example/x", "https://a.example/x"},
		{"https://a.example:8443/x/", "http://a.example:8443/x", "https://a.example:8443/x"},
		{"https://a.example:8443/x/", "http://a.example/x", "https://a.example:8443/x"},
		{"https://a.example/x/", "http://b.example/x", "http://b.example/x"},           // other host: unchanged
		{"https://a.example/x/", "http://a.example:9000/x", "http://a.example:9000/x"}, // other port: unchanged
		{"https://a.example/x/", "/x", "/x"},                                           // relative: unchanged
		{"http://a.example/x/", "http://a.example/x", "http://a.example/x"},            // not from https: unchanged
	} {
		rt := &redirectUpgradingRoundTripper{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {tc.location}}}, nil
		})}
		req, _ := http.NewRequest(http.MethodGet, tc.request, nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Location"); got != tc.want {
			t.Errorf("request %s, Location %s: got %s, want %s", tc.request, tc.location, got, tc.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOAuthFailedAuthorizationIsNotRetried(t *testing.T) {
	o := newOAuthSetup(t, func(s *testservers.AuthServer) { s.DenyAuthorization = true })
	events, res := o.tools(t)
	if res.err == nil || !strings.Contains(res.err.Error(), "access_denied") {
		t.Fatalf("want access_denied error, got %v", res.err)
	}
	if n := strings.Count(strings.Join(events, " "), "authorize"); n != 1 {
		t.Errorf("authorization attempted %d times, want 1 (events %q)", n, events)
	}
}

func TestOAuthSavedServer(t *testing.T) {
	o := newOAuthSetup(t, nil)
	config, _ := configDir()

	mustRun(t, "add", "fake", "--redirect-port", o.port, "--scope", "read", o.url)
	authPath := filepath.Join(config, "auth", "fake.json")
	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("no auth cache for saved server: %v", err)
	}
	if _, err := os.Stat(o.authPath); !os.IsNotExist(err) {
		t.Errorf("saved server should not use the URL cache %s", o.authPath)
	}
	saved, err := loadServer("fake")
	if err != nil || !slices.Equal(saved.Scopes, []string{"read"}) || saved.redirectPort() != mustAtoi(t, o.port) {
		t.Errorf("saved definition %+v, %v", saved, err)
	}

	if out := mustRun(t, "list"); !strings.Contains(out, "oauth: token valid until") {
		t.Errorf("list:\n%s", out)
	}

	o.auth.ResetEvents()
	mustRun(t, "tools", "fake")
	requireEvents(t, o.auth.Events())

	mustRun(t, "remove", "fake")
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Errorf("%s still exists after remove", authPath)
	}
}

func TestBearerCmdRerunsWhenTokenRejected(t *testing.T) {
	o := newOAuthSetup(t, nil)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, []byte(o.auth.IssueToken()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// First run prints a stale token, later runs the valid one.
	state := filepath.Join(dir, "state")
	cmd := fmt.Sprintf(`if [ -f %q ]; then cat %q; else touch %q; echo stale-token; fi`, state, valid, state)

	events, res := o.tools(t, "--bearer-cmd", cmd)
	requireSuccess(t, res)
	requireEvents(t, events, "access-rejected")
}

func TestStaticBearerRejected(t *testing.T) {
	o := newOAuthSetup(t, nil)
	_, res := o.tools(t, "--bearer", "wrong")
	if res.err == nil || !strings.Contains(res.err.Error(), "server rejected the bearer token") {
		t.Fatalf("got %v", res.err)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
