// Command schemaserver runs the schema test server over stdio, or over HTTP
// with -http (streamable) or -sse:
//
//	go build -o /tmp/schemaserver ./testdata/schemaserver
//	mcp-inspect tool search -- /tmp/schemaserver
package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/mikiobraun/mcp-inspect/internal/testservers"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	httpAddr := flag.String("http", "", "serve streamable HTTP at this address instead of stdio")
	sseAddr := flag.String("sse", "", "serve legacy SSE at this address instead of stdio")
	flag.Parse()

	server := testservers.NewSchemaServer()
	switch {
	case *httpAddr != "":
		log.Fatal(http.ListenAndServe(*httpAddr, testservers.NewStreamableHandler(server)))
	case *sseAddr != "":
		log.Fatal(http.ListenAndServe(*sseAddr, testservers.NewSSEHandler(server)))
	default:
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			log.Fatal(err)
		}
	}
}
