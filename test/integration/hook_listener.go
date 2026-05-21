package integration

import (
	"fmt"
	"net"
	"net/http"
	"testing"
)

// StartHookListener runs an HTTP server on a random port reachable from
// inside the container via host.docker.internal. It returns the URL to
// place in settings.json (POSTs from Claude hit handler). The server is
// stopped automatically when the test ends.
//
// The test owns the handler entirely: parsing, filtering, pausing, and
// release semantics are the test's concern. This helper only provides the
// HTTP transport.
func StartHookListener(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("hook listener listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return fmt.Sprintf("http://host.docker.internal:%d/hook", port)
}
