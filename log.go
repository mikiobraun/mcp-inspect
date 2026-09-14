package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Verbosity levels, set by repeating -v.
const (
	vLifecycle = 1 // connection steps, transport and auth decisions
	vJSONRPC   = 2 // every JSON-RPC message
	vHTTP      = 3 // raw HTTP requests and responses
)

var verbosity int

var logMu sync.Mutex

// logf writes a diagnostic line to stderr if verbosity is at least level.
func logf(level int, format string, args ...any) {
	if verbosity < level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	logMu.Lock()
	defer logMu.Unlock()
	prefix := strings.Repeat("v", level)
	for line := range strings.SplitSeq(strings.TrimRight(msg, "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "[%-3s] %s\n", prefix, line)
	}
}

// redact replaces a secret with a short hash, so different values can still be told apart.
func redact(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("<redacted sha256:%x, %d chars>", sum[:4], len(s))
}
