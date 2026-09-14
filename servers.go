package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

const defaultRedirectPort = 33418

// target describes how to reach a server. Saved servers are stored as
// servers/<name>.json; ad-hoc targets are built from arguments and flags.
// Exactly one of URL and Command is set.
type target struct {
	URL          string   `json:"url,omitempty"`
	Command      []string `json:"command,omitempty"`
	SSE          bool     `json:"sse,omitempty"`
	Headers      []string `json:"headers,omitempty"` // "Name: value"
	Bearer       string   `json:"bearer,omitempty"`
	BearerCmd    string   `json:"bearer_cmd,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	RedirectPort int      `json:"redirect_port,omitempty"`
}

func (t *target) redirectPort() int {
	if t.RedirectPort == 0 {
		return defaultRedirectPort
	}
	return t.RedirectPort
}

func (t *target) describe() string {
	if t.URL == "" {
		return "stdio: " + strings.Join(t.Command, " ")
	}
	if t.SSE {
		return t.URL + " (sse)"
	}
	return t.URL
}

// connectionFlags are the flags that describe a target. They apply to ad-hoc
// URL targets and to add, not to saved servers.
var connectionFlags = []string{"sse", "header", "bearer", "bearer-cmd", "client-id", "client-secret", "scope", "redirect-port"}

func bindConnectionFlags(cmd *cobra.Command, t *target) {
	pf := cmd.PersistentFlags()
	pf.BoolVar(&t.SSE, "sse", false, "use the legacy SSE transport instead of streamable HTTP")
	pf.StringArrayVarP(&t.Headers, "header", "H", nil, "extra HTTP header 'Name: value' (repeatable)")
	pf.StringVar(&t.Bearer, "bearer", "", "static bearer token")
	pf.StringVar(&t.BearerCmd, "bearer-cmd", "", "shell command printing a bearer token; re-run when the token is rejected")
	pf.StringVar(&t.ClientID, "client-id", "", "OAuth client id (skips dynamic client registration)")
	pf.StringVar(&t.ClientSecret, "client-secret", "", "OAuth client secret")
	pf.StringSliceVar(&t.Scopes, "scope", nil, "OAuth scopes to request instead of the discovered ones (repeatable or comma-separated)")
	pf.IntVar(&t.RedirectPort, "redirect-port", defaultRedirectPort, "local port for the OAuth callback")
}

func changedConnectionFlags(cmd *cobra.Command) []string {
	var changed []string
	for _, name := range connectionFlags {
		if cmd.Flags().Changed(name) {
			changed = append(changed, "--"+name)
		}
	}
	return changed
}

var serverNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func configDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mcp-inspect"), nil
}

func serverPath(name string) (string, error) {
	if !serverNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid server name %q: use letters, digits, '.', '_', '-'", name)
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "servers", name+".json"), nil
}

func namedAuthPath(name string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "auth", name+".json"), nil
}

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// urlAuthPath is the auth cache for an ad-hoc URL target, e.g. auth/url/kb.miki.one_mcp.json.
func urlAuthPath(serverURL string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	name := strings.Trim(unsafePathChars.ReplaceAllString(strings.SplitN(serverURL, "://", 2)[1], "_"), "_")
	return filepath.Join(dir, "auth", "url", name+".json"), nil
}

func loadServer(name string) (*target, error) {
	path, err := serverPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no saved server %q (see 'mcp-inspect list')", name)
	}
	if err != nil {
		return nil, err
	}
	var t target
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if (t.URL == "") == (len(t.Command) == 0) {
		return nil, fmt.Errorf("%s: exactly one of url and command must be set", path)
	}
	return &t, nil
}

// saveServer writes the definition readable only by the user: it may contain
// bearer tokens, client secrets or auth headers.
func saveServer(path string, t *target) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// adHocTarget builds a target from a URL or a stdio command line and the
// connection flags. args are the target arguments; dash is the index of "--"
// within them, or -1.
func adHocTarget(cmd *cobra.Command, args []string, dash int, flags *target) (*target, error) {
	t := *flags
	if !cmd.Flags().Changed("redirect-port") {
		t.RedirectPort = 0
	}
	switch {
	case dash == 0:
		if len(args) == 0 {
			return nil, errors.New("missing command after --")
		}
		if changed := changedConnectionFlags(cmd); len(changed) > 0 {
			return nil, fmt.Errorf("connection flags (%s) only apply to URL targets", strings.Join(changed, ", "))
		}
		t.Command = args
	case len(args) == 1 && isURL(args[0]):
		t.URL = args[0]
	default:
		return nil, fmt.Errorf("expected a URL or -- followed by a command, got %q", args)
	}
	return &t, nil
}

// resolveTarget interprets the target arguments of a command: a saved server
// name, a URL, or -- followed by a stdio command. It returns the target and
// its auth cache path.
func resolveTarget(cmd *cobra.Command, args []string, dash int, flags *target) (*target, string, error) {
	if len(args) == 1 && dash != 0 && !isURL(args[0]) {
		name := args[0]
		if changed := changedConnectionFlags(cmd); len(changed) > 0 {
			path, _ := serverPath(name)
			return nil, "", fmt.Errorf("connection flags (%s) don't apply to saved server %q; edit %s to change it", strings.Join(changed, ", "), name, path)
		}
		t, err := loadServer(name)
		if err != nil {
			return nil, "", err
		}
		authPath, err := namedAuthPath(name)
		return t, authPath, err
	}
	if len(args) == 0 {
		return nil, "", errors.New("missing target: a saved server name, a URL, or -- followed by a command")
	}
	t, err := adHocTarget(cmd, args, dash, flags)
	if err != nil {
		return nil, "", err
	}
	if t.URL == "" {
		return t, "", nil
	}
	authPath, err := urlAuthPath(t.URL)
	return t, authPath, err
}

// targetArgs drops the first n positional arguments and adjusts the position of "--".
func targetArgs(cmd *cobra.Command, args []string, n int) ([]string, int, error) {
	dash := cmd.ArgsLenAtDash()
	if dash >= 0 && dash < n {
		return nil, 0, fmt.Errorf("expected %d argument(s) before --", n)
	}
	if dash >= 0 {
		dash -= n
	}
	return args[n:], dash, nil
}

func addCmd(flags *target) *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> <url> | add <name> -- <command> [args...]",
		Short: "Save a server under a name, connecting once (runs OAuth if required)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path, err := serverPath(name)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("server %q already exists (%s); remove it first", name, path)
			}
			rest, dash, err := targetArgs(cmd, args, 1)
			if err != nil {
				return err
			}
			t, err := adHocTarget(cmd, rest, dash, flags)
			if err != nil {
				return err
			}
			authPath, err := namedAuthPath(name)
			if err != nil {
				return err
			}
			return withSession(cmd.OutOrStdout(), t, authPath, func(_ context.Context, w io.Writer, session *mcp.ClientSession, _ *rawResults) error {
				if err := saveServer(path, t); err != nil {
					return err
				}
				printServerHeader(w, session.InitializeResult())
				fmt.Fprintf(w, "saved server %q to %s\n", name, path)
				return nil
			})
		},
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := configDir()
			if err != nil {
				return err
			}
			files, err := filepath.Glob(filepath.Join(dir, "servers", "*.json"))
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no saved servers (see 'mcp-inspect add')")
				return nil
			}
			slices.Sort(files)
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, f := range files {
				name := strings.TrimSuffix(filepath.Base(f), ".json")
				t, err := loadServer(name)
				if err != nil {
					fmt.Fprintf(w, "%s\t<error: %v>\t\n", name, err)
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", name, t.describe(), authStatus(name, t))
			}
			return w.Flush()
		},
	}
}

func authStatus(name string, t *target) string {
	switch {
	case t.URL == "":
		return "-"
	case t.Bearer != "":
		return "bearer"
	case t.BearerCmd != "":
		return "bearer-cmd"
	case slices.ContainsFunc(t.Headers, func(h string) bool {
		k, _, _ := strings.Cut(h, ":")
		return strings.EqualFold(strings.TrimSpace(k), "Authorization")
	}):
		return "authorization header"
	}
	path, err := namedAuthPath(name)
	if err != nil {
		return "oauth: " + err.Error()
	}
	s, err := loadStoredAuth(path)
	switch {
	case err != nil:
		return "oauth: " + err.Error()
	case s == nil:
		return "oauth: not authorized (or not required)"
	case s.Token == nil:
		return "oauth: no token"
	case s.Token.Valid():
		return "oauth: token valid until " + s.Token.Expiry.Local().Format(time.DateTime)
	case s.Token.RefreshToken != "":
		return "oauth: token expired, refreshable"
	default:
		return "oauth: token expired"
	}
}

func removeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a saved server and its cached auth",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path, err := serverPath(name)
			if err != nil {
				return err
			}
			if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("no saved server %q", name)
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", path)
			authPath, err := namedAuthPath(name)
			if err != nil {
				return err
			}
			if err := os.Remove(authPath); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", authPath)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
	}
}
