package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikiobraun/mcp-inspect/internal/testservers"
)

func TestToolsOverStdio(t *testing.T) {
	isolateConfig(t)
	out := mustRun(t, args("tools", stdioTarget(t))...)
	for _, want := range []string{"schemas 1.0.0", "A server for testing schema rendering.", "4 tools", "search (Search things)", "echo\n  Return the arguments."} {
		if !strings.Contains(out, want) {
			t.Errorf("tools output lacks %q:\n%s", want, out)
		}
	}
}

func TestInfoOverStreamableHTTP(t *testing.T) {
	isolateConfig(t)
	srv := httptest.NewServer(testservers.NewStreamableHandler(testservers.NewSchemaServer()))
	defer srv.Close()
	out := mustRun(t, "info", srv.URL)
	if !strings.Contains(out, "schemas 1.0.0") || !strings.Contains(out, "capabilities: tools") {
		t.Errorf("unexpected info output:\n%s", out)
	}
}

func TestToolsOverSSE(t *testing.T) {
	isolateConfig(t)
	srv := httptest.NewServer(testservers.NewSSEHandler(testservers.NewSchemaServer()))
	defer srv.Close()
	out := mustRun(t, "tools", "--sse", srv.URL)
	if !strings.Contains(out, "4 tools") {
		t.Errorf("unexpected tools output:\n%s", out)
	}
}

func TestToolRendering(t *testing.T) {
	isolateConfig(t)
	checkGolden(t, "tool_search", mustRun(t, args("tool", "search", stdioTarget(t))...))
	checkGolden(t, "tool_noargs", mustRun(t, args("tool", "noargs", stdioTarget(t))...))
}

func TestToolJSONKeepsServerOrder(t *testing.T) {
	isolateConfig(t)
	out := mustRun(t, args("tool", "search", "--json", stdioTarget(t))...)
	order := []string{`"query"`, `"mode"`, `"limit"`, `"filter"`, `"target"`, `"tags"`, `"anything"`}
	last := -1
	for _, key := range order {
		i := strings.Index(out, key)
		if i <= last {
			t.Fatalf("property %s out of order in:\n%s", key, out)
		}
		last = i
	}
}

func TestToolUnknown(t *testing.T) {
	isolateConfig(t)
	res := runCLI(t, "", args("tool", "nope", stdioTarget(t))...)
	// The go-sdk server lists tools sorted by name.
	if res.err == nil || !strings.Contains(res.err.Error(), `no tool "nope"; available: echo, fail, noargs, search`) {
		t.Errorf("got error %v", res.err)
	}
}

func TestCallWithStdinArgs(t *testing.T) {
	isolateConfig(t)
	res := runCLI(t, `{"b": 1, "a": "x"}`, args("call", "echo", "--args", "-", stdioTarget(t))...)
	if res.err != nil {
		t.Fatalf("call: %v", res.err)
	}
	var result struct {
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &result); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, res.stdout)
	}
	if result.StructuredContent["a"] != "x" || result.StructuredContent["b"] != 1.0 || result.IsError {
		t.Errorf("unexpected result: %s", res.stdout)
	}
}

func TestCallToolErrorExitsWith2(t *testing.T) {
	isolateConfig(t)
	res := runCLI(t, "", args("call", "fail", stdioTarget(t))...)
	ee, ok := errors.AsType[*exitError](res.err)
	if !ok || ee.code != 2 {
		t.Fatalf("want exit code 2, got error %v", res.err)
	}
	if !strings.Contains(res.stdout, `"isError": true`) || !strings.Contains(res.stdout, "this tool always fails") {
		t.Errorf("result not printed:\n%s", res.stdout)
	}
}

func TestCallUnknownToolIsProtocolError(t *testing.T) {
	isolateConfig(t)
	res := runCLI(t, "", args("call", "nope", stdioTarget(t))...)
	if res.err == nil {
		t.Fatal("want error")
	}
	if _, ok := errors.AsType[*exitError](res.err); ok {
		t.Errorf("unknown tool should not be a tool error: %v", res.err)
	}
}

func TestReadToolArgs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "args.json")
	if err := os.WriteFile(file, []byte(` {"from": "file"} `), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flag, stdin, want, err string
	}{
		{flag: "", want: `{}`},
		{flag: `{"a":1}`, want: `{"a":1}`},
		{flag: "@" + file, want: `{"from": "file"}`},
		{flag: "-", stdin: "{\"from\": \"stdin\"}\n", want: `{"from": "stdin"}`},
		{flag: `[1]`, err: "must be a JSON object"},
		{flag: `{nope`, err: "not valid JSON"},
		{flag: "@/does/not/exist", err: "reading --args"},
	} {
		got, err := readToolArgs(tc.flag, strings.NewReader(tc.stdin))
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("readToolArgs(%q): got error %v, want %q", tc.flag, err, tc.err)
		case tc.err == "" && (err != nil || string(got) != tc.want):
			t.Errorf("readToolArgs(%q) = %s, %v; want %s", tc.flag, got, err, tc.want)
		}
	}
}

func TestSavedServers(t *testing.T) {
	config := isolateConfig(t)
	target := stdioTarget(t)

	out := mustRun(t, args("add", "schemas", target)...)
	if !strings.Contains(out, `saved server "schemas"`) {
		t.Errorf("add output:\n%s", out)
	}
	path := filepath.Join(config, "servers", "schemas.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s has mode %v, want 0600", path, info.Mode().Perm())
	}

	if res := runCLI(t, "", args("add", "schemas", target)...); res.err == nil || !strings.Contains(res.err.Error(), "already exists") {
		t.Errorf("second add: %v", res.err)
	}

	if out := mustRun(t, "tools", "schemas"); !strings.Contains(out, "4 tools") {
		t.Errorf("tools by name:\n%s", out)
	}
	if out := mustRun(t, "list"); !strings.Contains(out, "schemas  stdio: "+os.Args[0]) {
		t.Errorf("list:\n%s", out)
	}

	res := runCLI(t, "", "tools", "schemas", "--sse")
	if res.err == nil || !strings.Contains(res.err.Error(), `connection flags (--sse) don't apply to saved server "schemas"`) {
		t.Errorf("flags with saved name: %v", res.err)
	}

	mustRun(t, "remove", "schemas")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists after remove", path)
	}
	if res := runCLI(t, "", "tools", "schemas"); res.err == nil || !strings.Contains(res.err.Error(), `no saved server "schemas"`) {
		t.Errorf("tools after remove: %v", res.err)
	}
}

func TestTargetErrors(t *testing.T) {
	isolateConfig(t)
	for _, tc := range []struct {
		args []string
		err  string
	}{
		{args("tools"), "missing target"},
		{args("tools", "--bearer", "x", stdioTarget(t)), "connection flags (--bearer) only apply to URL targets"},
		{args("tools", "a", "b"), `expected a URL or -- followed by a command`},
		{args("add", "bad/name", "http://127.0.0.1:1/mcp"), "invalid server name"},
		{args("tools", "--bearer", "x", "--bearer-cmd", "y", "http://127.0.0.1:1/mcp"), "mutually exclusive"},
	} {
		res := runCLI(t, "", tc.args...)
		if res.err == nil || !strings.Contains(res.err.Error(), tc.err) {
			t.Errorf("mcp-inspect %s: got error %v, want %q", strings.Join(tc.args, " "), res.err, tc.err)
		}
	}
}
