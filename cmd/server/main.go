// Command server serves the website and its API, and runs syncs on request.
//
//	go run ./cmd/server -dir data -addr :8080
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ella.to/movies/internal/rsync"
	"ella.to/movies/internal/server"
	"ella.to/movies/internal/source"
	"ella.to/movies/internal/source/diycraftsguide"
	"ella.to/movies/internal/source/farsiplex"
)

func main() {
	dir := flag.String("dir", "data", "data directory: each source's files, the merged catalog and the sync status")
	addr := flag.String("addr", ":8080", "address to listen on")
	flag.Parse()
	log.SetFlags(log.Ltime)

	// The first source wins for a title's display fields when sources overlap.
	sources := []source.Source{diycraftsguide.New(), farsiplex.New()}
	syncer, err := rsync.New(*dir, sources...)
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{Addr: *addr, Handler: server.New(syncer)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Print("shutting down…")
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("serving %s on %s", *dir, *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	syncer.Close()
}
