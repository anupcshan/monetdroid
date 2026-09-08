package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
)

func main() {
	addr := flag.String("addr", ":8222", "listen address")
	trace := flag.Bool("trace", false, "enable git trace logging")
	claudeBin := flag.String("claude-bin", "", `path to the claude CLI executable. For a complex invocation, write a wrapper script and pass its path. Defaults to "claude" in PATH.`)
	modelDir := flag.String("model-dir", "", "directory scanned for model wrappers, offered in the new-session model picker")
	modelPattern := flag.String("model-pattern", monetdroid.DefaultModelPattern, "regex filtering scanned model names. The first capture group becomes the picker label.")
	flag.Parse()
	monetdroid.SetTraceEnabled(*trace)

	hub, err := monetdroid.NewHub(httpURL(*addr, "127.0.0.1"), *claudeBin, monetdroid.ModelScanSpec{
		Dir:     *modelDir,
		Pattern: *modelPattern,
	})
	if err != nil {
		log.Fatalf("startup: %s", err)
	}
	mux := monetdroid.RegisterRoutes(hub)

	log.Printf("Monet Droid listening on %s", *addr)
	log.Printf("open %s", httpURL(*addr, "localhost"))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// httpURL builds an http:// URL targeting host on the port extracted from
// addr. addr may be ":port", "host:port", "[ipv6]:port", or a bare port.
func httpURL(addr, host string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = addr
	}
	return "http://" + host + ":" + port
}
