package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// loggingRoundTripper logs raw HTTP traffic at -vvv.
// Secrets in headers and in JSON bodies (token responses, client registration) are redacted.
type loggingRoundTripper struct {
	next http.RoundTripper
}

var secretHeaders = []string{"Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization"}

var secretJSONFields = []string{"access_token", "refresh_token", "id_token", "client_secret"}

var secretFormFields = append([]string{"code", "code_verifier"}, secretJSONFields...)

func (t *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if verbosity < vHTTP {
		return t.next.RoundTrip(req)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "→ %s %s\n", req.Method, req.URL)
	writeHeaders(&b, req.Header)
	if req.Body != nil && req.GetBody != nil {
		body, err := req.GetBody()
		if err == nil {
			data, _ := io.ReadAll(body)
			writeBody(&b, req.Header.Get("Content-Type"), data)
		}
	}
	logf(vHTTP, "%s", b.String())

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		logf(vHTTP, "← %s %s: error: %v", req.Method, req.URL, err)
		return nil, err
	}

	b.Reset()
	fmt.Fprintf(&b, "← %s (%s %s)\n", resp.Status, req.Method, req.URL)
	writeHeaders(&b, resp.Header)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Streams stay open; their messages show up at -vv as JSON-RPC.
		b.WriteString("  <event stream, see JSON-RPC log>\n")
	} else {
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
		writeBody(&b, resp.Header.Get("Content-Type"), data)
	}
	logf(vHTTP, "%s", b.String())
	return resp, nil
}

func writeHeaders(b *strings.Builder, h http.Header) {
	for _, k := range slices.Sorted(maps.Keys(h)) {
		for _, v := range h[k] {
			if slices.Contains(secretHeaders, k) {
				v = redactHeaderValue(v)
			}
			fmt.Fprintf(b, "  %s: %s\n", k, v)
		}
	}
}

// redactHeaderValue keeps the auth scheme ("Bearer", "Basic") visible.
func redactHeaderValue(v string) string {
	scheme, secret, ok := strings.Cut(v, " ")
	if !ok {
		return redact(v)
	}
	return scheme + " " + redact(secret)
}

func writeBody(b *strings.Builder, contentType string, data []byte) {
	if len(data) == 0 {
		return
	}
	switch {
	case strings.HasPrefix(contentType, "application/json"):
		var v any
		if err := json.Unmarshal(data, &v); err == nil {
			redactJSON(v)
			data, _ = json.MarshalIndent(v, "  ", "  ")
		}
	case strings.HasPrefix(contentType, "application/x-www-form-urlencoded"):
		data = []byte(redactForm(string(data)))
	}
	fmt.Fprintf(b, "\n  %s\n", data)
}

func redactJSON(v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok && slices.Contains(secretJSONFields, k) {
				v[k] = redact(s)
			} else {
				redactJSON(val)
			}
		}
	case []any:
		for _, val := range v {
			redactJSON(val)
		}
	}
}

func redactForm(s string) string {
	parts := strings.Split(s, "&")
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if ok && slices.Contains(secretFormFields, k) {
			parts[i] = k + "=" + redact(v)
		}
	}
	return strings.Join(parts, "&")
}
