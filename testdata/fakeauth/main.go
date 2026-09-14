// Command fakeauth runs a fake OAuth authorization server with a protected MCP
// endpoint at /mcp, for trying mcp-inspect's auth flows by hand:
//
//	go run ./testdata/fakeauth -addr 127.0.0.1:18090
//	mcp-inspect -v tools http://127.0.0.1:18090/mcp
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/mikiobraun/mcp-inspect/internal/testservers"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address")
	lifetime := flag.Duration("token-lifetime", time.Hour, "access token lifetime")
	trailingSlash := flag.Bool("trailing-slash", false, `advertise the resource with a trailing "/"`)
	deny := flag.Bool("deny", false, "deny every authorization request")
	flag.Parse()

	s := testservers.NewAuthServer()
	s.TokenLifetime = *lifetime
	s.DenyAuthorization = *deny
	if *trailingSlash {
		s.ResourceSuffix = "/"
	}
	s.Logf = log.Printf

	log.Printf("serving on http://%s/mcp", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
