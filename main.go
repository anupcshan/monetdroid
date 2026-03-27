package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
)

func main() {
	addr := flag.String("addr", ":8222", "listen address")
	trace := flag.Bool("trace", false, "enable git trace logging")
	flag.Parse()
	monetdroid.SetTraceEnabled(*trace)

	hub := monetdroid.NewHub()
	mux := monetdroid.RegisterRoutes(hub)

	log.Printf("Monet Droid listening on %s", *addr)
	log.Printf("open http://localhost%s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
