// Command basic is a small JSON API for the flashpoint example: run
// `flashpoint` in this directory and edit the message below.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const message = "Hello from Go"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	started := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message": message,
			"pid":     os.Getpid(),
			"started": started.Format(time.RFC3339Nano),
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("listening on %s", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	// Serve returns as soon as Shutdown starts; exiting then would cut off
	// the requests still in flight.
	<-drained
	log.Print("stopped")
}
