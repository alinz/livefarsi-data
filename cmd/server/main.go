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

	"farsi.ella.to/internal/rsync"
	"farsi.ella.to/internal/server"
	"farsi.ella.to/internal/source"
	"farsi.ella.to/internal/source/diycraftsguide"
	"farsi.ella.to/internal/source/farsiplex"
	"farsi.ella.to/internal/source/fmoviess"
)

func main() {
	dir := flag.String("dir", "data", "data directory: holds the database, movies.db")
	addr := flag.String("addr", ":8080", "address to listen on")
	flag.Parse()
	log.SetFlags(log.Ltime)

	// Sources are grouped by their section. Within a section, the first source
	// wins for a title's display fields when sources overlap.
	sources := []source.Source{diycraftsguide.New(), farsiplex.New(), fmoviess.New()}
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
