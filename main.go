package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		if ee, ok := errors.AsType[*exitError](err); ok {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var flags target

	root := &cobra.Command{
		Use:   "mcp-inspect",
		Short: "Inspect MCP servers",
		Long: `Inspect MCP servers over stdio, streamable HTTP or legacy SSE.

Targets:
  mcp-inspect tools kb                                saved server (see add, list)
  mcp-inspect tools https://example.com/mcp           streamable HTTP
  mcp-inspect tools --sse https://example.com/sse     legacy SSE
  mcp-inspect tools -- npx -y some-mcp-server arg     stdio

Saved servers:
  mcp-inspect add kb https://kb.miki.one [connection flags]
  mcp-inspect add fs -- npx -y some-mcp-server arg
  The definition, including connection flags, is saved to
  mcp-inspect/servers/<name>.json in the user config directory (mode 0600).

Auth (URL targets):
  Without --bearer, --bearer-cmd or an Authorization header, a 401 from the
  server starts the OAuth flow. Clients and tokens are cached under
  mcp-inspect/auth/<name>.json for saved servers, and mcp-inspect/auth/url/
  for URLs.

Verbosity:
  -v    connection lifecycle, auth decisions, stdio server stderr
  -vv   + JSON-RPC messages
  -vvv  + raw HTTP requests and responses (secrets redacted)`,
		SilenceUsage: true,
	}

	root.PersistentFlags().CountVarP(&verbosity, "verbose", "v", "increase verbosity (-v, -vv, -vvv)")
	bindConnectionFlags(root, &flags)

	root.AddCommand(toolsCmd(&flags), toolCmd(&flags), callCmd(&flags), infoCmd(&flags), addCmd(&flags), listCmd(), removeCmd())
	return root
}

const targetUsage = "<name> | <url> | -- <command> [args...]"

// sessionRunE resolves the command's target arguments and runs f with a session.
func sessionRunE(flags *target, f sessionFunc) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		t, authPath, err := resolveTarget(cmd, args, cmd.ArgsLenAtDash(), flags)
		if err != nil {
			return err
		}
		return withSession(t, authPath, f)
	}
}

func toolsCmd(flags *target) *cobra.Command {
	var schema, asJSON bool
	cmd := &cobra.Command{
		Use:   "tools " + targetUsage,
		Short: "List tools with their descriptions",
		RunE: sessionRunE(flags, func(ctx context.Context, session *mcp.ClientSession, _ *rawResults) error {
			var tools []*mcp.Tool
			for tool, err := range session.Tools(ctx, nil) {
				if err != nil {
					return fmt.Errorf("listing tools: %w", err)
				}
				tools = append(tools, tool)
			}
			if asJSON {
				return printJSON(tools)
			}
			printServerHeader(session.InitializeResult())
			fmt.Printf("%d tools\n", len(tools))
			for _, tool := range tools {
				fmt.Println()
				name := tool.Name
				if tool.Title != "" && tool.Title != tool.Name {
					name += " (" + tool.Title + ")"
				}
				fmt.Println(name)
				printIndented(tool.Description, "  ")
				if schema {
					data, err := json.MarshalIndent(tool.InputSchema, "", "  ")
					if err != nil {
						return err
					}
					fmt.Println("  input schema:")
					printIndented(string(data), "    ")
				}
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&schema, "schema", false, "show input schemas")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the tool list as JSON")
	return cmd
}

func infoCmd(flags *target) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info " + targetUsage,
		Short: "Show server info, capabilities and instructions",
		RunE: sessionRunE(flags, func(ctx context.Context, session *mcp.ClientSession, _ *rawResults) error {
			res := session.InitializeResult()
			if asJSON {
				return printJSON(res)
			}
			printServerHeader(res)
			fmt.Printf("protocol:     %s\n", res.ProtocolVersion)
			fmt.Printf("capabilities: %s\n", describeCapabilities(res.Capabilities))
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the initialize result as JSON")
	return cmd
}

type sessionFunc func(ctx context.Context, session *mcp.ClientSession, raw *rawResults) error

func withSession(t *target, authPath string, f sessionFunc) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	raw := newRawResults()
	session, err := connect(ctx, t, authPath, raw)
	if err != nil {
		return err
	}
	defer session.Close()
	return f(ctx, session, raw)
}

func printServerHeader(res *mcp.InitializeResult) {
	info := res.ServerInfo
	title := info.Name
	if info.Title != "" {
		title = info.Title + " [" + info.Name + "]"
	}
	fmt.Println(strings.TrimSpace(title + " " + info.Version))
	if info.WebsiteURL != "" {
		fmt.Println(info.WebsiteURL)
	}
	if info.Description != "" {
		printIndented(info.Description, "  ")
	}
	if res.Instructions != "" {
		fmt.Println("\ninstructions:")
		printIndented(res.Instructions, "  ")
	}
	fmt.Println()
}

func describeCapabilities(c *mcp.ServerCapabilities) string {
	if c == nil {
		return "none"
	}
	var parts []string
	withFlags := func(name string, flags ...string) {
		if len(flags) > 0 {
			name += " (" + strings.Join(flags, ", ") + ")"
		}
		parts = append(parts, name)
	}
	flag := func(set bool, name string) []string {
		if set {
			return []string{name}
		}
		return nil
	}
	if c.Tools != nil {
		withFlags("tools", flag(c.Tools.ListChanged, "listChanged")...)
	}
	if c.Resources != nil {
		withFlags("resources", append(flag(c.Resources.Subscribe, "subscribe"), flag(c.Resources.ListChanged, "listChanged")...)...)
	}
	if c.Prompts != nil {
		withFlags("prompts", flag(c.Prompts.ListChanged, "listChanged")...)
	}
	if c.Logging != nil {
		parts = append(parts, "logging")
	}
	if c.Completions != nil {
		parts = append(parts, "completions")
	}
	for k := range c.Experimental {
		parts = append(parts, "experimental:"+k)
	}
	for k := range c.Extensions {
		parts = append(parts, "extension:"+k)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func printIndented(s, indent string) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	for line := range strings.SplitSeq(s, "\n") {
		fmt.Println(strings.TrimRight(indent+line, " "))
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
