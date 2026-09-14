package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mikiobraun/mcp-inspect/internal/testservers"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestSchemaParsingKeepsOrder(t *testing.T) {
	var s schema
	if err := json.Unmarshal([]byte(testservers.SearchInputSchema), &s); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range s.Properties {
		names = append(names, p.Name)
	}
	if want := []string{"query", "mode", "limit", "filter", "target", "tags", "anything"}; !slices.Equal(names, want) {
		t.Errorf("properties %v, want %v", names, want)
	}
	if len(s.Defs) != 1 || s.Defs[0].Name != "Filter" || s.Defs[0].Schema.Properties[1].Name != "since" {
		t.Errorf("unexpected $defs: %+v", s.Defs)
	}
}

func TestTypeSummary(t *testing.T) {
	for _, tc := range []struct {
		schema   string
		required bool
		want     string
	}{
		{`{"type": "string"}`, true, "string, required"},
		{`{"type": ["null", "array"], "items": {"type": "object"}}`, false, "array of object, nullable"},
		{`{"type": ["string", "integer"]}`, false, "string|integer"},
		{`{"$ref": "#/$defs/X"}`, false, "$ref #/$defs/X"},
		{`{"anyOf": [{"type": "string"}]}`, false, "anyOf"},
		{`{"properties": {"a": {}}}`, false, "object"},
		{`{}`, false, "any"},
		{`true`, false, "any"},
		{`false`, false, "never"},
	} {
		var s schema
		if err := json.Unmarshal([]byte(tc.schema), &s); err != nil {
			t.Fatalf("%s: %v", tc.schema, err)
		}
		if got := typeSummary(&s, tc.required); got != tc.want {
			t.Errorf("typeSummary(%s) = %q, want %q", tc.schema, got, tc.want)
		}
	}
}

func TestPrintWrapped(t *testing.T) {
	var b strings.Builder
	printWrapped(&b, strings.Repeat("word ", 20)+"\n\nsecond", 4)
	want := "    word word word word word word word word word word word word word word word\n" +
		"    word word word word word\n" +
		"\n" +
		"    second\n"
	if b.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", b.String(), want)
	}
}

func TestRawResultsMatchesResponsesToRequests(t *testing.T) {
	id := func(n float64) jsonrpc.ID {
		id, err := jsonrpc.MakeID(n)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	r := newRawResults()
	r.request(&jsonrpc.Request{ID: id(1), Method: "tools/list"})
	r.request(&jsonrpc.Request{ID: id(2), Method: "tools/call"})
	r.request(&jsonrpc.Request{ID: id(3), Method: "tools/list"})
	r.response(&jsonrpc.Response{ID: id(3), Result: json.RawMessage(`"page2"`)})
	r.response(&jsonrpc.Response{ID: id(2), Error: &jsonrpc.Error{Code: 1, Message: "boom"}})
	r.response(&jsonrpc.Response{ID: id(1), Result: json.RawMessage(`"page1"`)})
	r.response(&jsonrpc.Response{ID: id(9), Result: json.RawMessage(`"unknown"`)})

	var pages []string
	for _, p := range r.get("tools/list") {
		pages = append(pages, string(p))
	}
	if want := []string{`"page2"`, `"page1"`}; !slices.Equal(pages, want) {
		t.Errorf("tools/list results %v, want %v (arrival order)", pages, want)
	}
	if got := r.get("tools/call"); len(got) != 0 {
		t.Errorf("error responses should not be recorded, got %s", got)
	}
}

func TestRedaction(t *testing.T) {
	secret := "eyJhbGciOiJSUzI1NiJ9.payload.signature"
	if got := redactHeaderValue("Bearer " + secret); !strings.HasPrefix(got, "Bearer <redacted sha256:") || strings.Contains(got, secret) {
		t.Errorf("header: %q", got)
	}
	if redact(secret) == redact(secret+"x") {
		t.Error("different secrets should redact differently")
	}

	var b strings.Builder
	writeBody(&b, "application/json", []byte(`{"access_token":"`+secret+`","nested":{"refresh_token":"`+secret+`"},"expires_in":3600,"code":"kept"}`))
	if strings.Contains(b.String(), secret) || !strings.Contains(b.String(), `"expires_in": 3600`) || !strings.Contains(b.String(), `"code": "kept"`) {
		t.Errorf("JSON body:\n%s", b.String())
	}

	form := redactForm("grant_type=authorization_code&code=" + secret + "&code_verifier=" + secret + "&client_id=abc")
	if strings.Contains(form, secret) || !strings.Contains(form, "client_id=abc") || !strings.Contains(form, "grant_type=authorization_code") {
		t.Errorf("form: %s", form)
	}
}

func TestURLAuthPath(t *testing.T) {
	isolateConfig(t)
	for url, want := range map[string]string{
		"https://kb.example.com":      "kb.example.com.json",
		"https://kb.example.com/":     "kb.example.com.json",
		"http://127.0.0.1:18090/mcp":  "127.0.0.1_18090_mcp.json",
		"https://example.com/a/b?x=1": "example.com_a_b_x_1.json",
	} {
		path, err := urlAuthPath(url)
		if err != nil || !strings.HasSuffix(path, "/auth/url/"+want) {
			t.Errorf("urlAuthPath(%q) = %q, %v; want suffix %q", url, path, err, want)
		}
	}
}
