package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikiobraun/mcp-inspect/internal/testservers"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdioServerEnv makes the test binary act as the schema test server over
// stdio, so stdio targets can run os.Args[0] without building anything.
const stdioServerEnv = "MCP_INSPECT_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		if err := testservers.NewSchemaServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// stdioTarget returns target arguments for the schema test server over stdio.
func stdioTarget(t *testing.T) []string {
	t.Setenv(stdioServerEnv, "1") // inherited by the server process
	return []string{"--", os.Args[0]}
}

// isolateConfig points the user config directory at a temporary directory and
// returns mcp-inspect's config dir inside it.
func isolateConfig(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir) // macOS derives the config dir from HOME
	configDir, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	return configDir
}

type cliResult struct {
	stdout string
	stderr string
	err    error
}

// runCLI runs mcp-inspect in-process with args and stdin.
func runCLI(t *testing.T, stdin string, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := rootCmd()
	root.SetArgs(args)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// mustRun runs mcp-inspect and fails the test on error.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	res := runCLI(t, "", args...)
	if res.err != nil {
		t.Fatalf("mcp-inspect %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.err, res.stdout, res.stderr)
	}
	return res.stdout
}

func args(parts ...any) []string {
	var out []string
	for _, p := range parts {
		switch p := p.(type) {
		case string:
			out = append(out, p)
		case []string:
			out = append(out, p...)
		}
	}
	return out
}

// checkGolden compares got with testdata/<name>.golden; run with
// MCP_INSPECT_UPDATE_GOLDEN=1 to rewrite it.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("MCP_INSPECT_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with MCP_INSPECT_UPDATE_GOLDEN=1 to create)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}
