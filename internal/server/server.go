// Package server is the HTTP API and website. Every /rsync and /api route
// takes ?section=farsi|foreign (default farsi).
//
//	GET /                 the website (web/index.html)
//	GET /api/sections     the section names
//	GET /rsync/start      start the section's full sync (?deep=1 re-checks every title); 202, or 409 if one is running
//	GET /rsync/status     the section's current or last sync status
//	GET /rsync/data       the section's title list as JSON, gzip-compressed
//	GET /api/title        one title (?kind=series|movies&id=...), with its seasons and players;
//	                      a title known only from a listing is synced first
//	GET /rsync/title      sync one title (?kind=...&id=...&deep=1), then return it; 409 while the
//	                      section's full sync runs
//	GET /config           baker routing config for farsi.ella.to (see config.go)
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/rsync"
	"farsi.ella.to/internal/source"
	"farsi.ella.to/web"
)

type Server struct {
	sync *rsync.Syncer
}

func New(s *rsync.Syncer) http.Handler {
	srv := &Server{sync: s}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.index)
	mux.HandleFunc("GET /api/sections", srv.sections)
	mux.HandleFunc("GET /api/title", srv.title)
	mux.HandleFunc("GET /rsync/start", srv.start)
	mux.HandleFunc("GET /rsync/status", srv.status)
	mux.HandleFunc("GET /rsync/data", srv.data)
	mux.HandleFunc("GET /rsync/title", srv.syncTitle)
	mux.HandleFunc("GET /config", srv.config)
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(web.Index)
}

// section resolves ?section=, writing a 404 if it's unknown.
func (s *Server) section(w http.ResponseWriter, r *http.Request) *rsync.Section {
	name := r.URL.Query().Get("section")
	if name == "" {
		name = source.Farsi
	}
	sec, err := s.sync.Section(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return nil
	}
	return sec
}

func flag(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "1" || v == "true"
}

func (s *Server) sections(w http.ResponseWriter, r *http.Request) {
	b, _ := json.Marshal(s.sync.Sections())
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	sec := s.section(w, r)
	if sec == nil {
		return
	}
	code := http.StatusAccepted
	if !sec.Start(flag(r, "deep")) {
		code = http.StatusConflict // already running
	}
	writeJSON(w, code, sec.Status())
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if sec := s.section(w, r); sec != nil {
		writeJSON(w, http.StatusOK, sec.Status())
	}
}

// data serves the section's title list. It is kept gzipped in memory; the
// rare client that doesn't accept gzip gets it decompressed on the fly.
func (s *Server) data(w http.ResponseWriter, r *http.Request) {
	sec := s.section(w, r)
	if sec == nil {
		return
	}
	d := sec.Data()
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", "Accept-Encoding")
	h.Set("ETag", d.ETag)
	if r.Header.Get("If-None-Match") == d.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		w.Write(d.Gzip)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(d.Gzip))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(w, zr); err != nil {
		log.Printf("GET /rsync/data: %v", err)
	}
}

// titleResponse is a title, plus the error of a sync that failed (the title
// is then as last saved).
type titleResponse struct {
	Title *catalog.Title `json:"title"`
	Error string         `json:"error,omitempty"`
}

// title returns one title. A partial one (known only from a listing) is
// synced first, waiting up to two minutes; if that fails it is returned as is,
// with the error.
func (s *Server) title(w http.ResponseWriter, r *http.Request) {
	sec := s.section(w, r)
	if sec == nil {
		return
	}
	q := r.URL.Query()
	kind, id := q.Get("kind"), q.Get("id")
	t := sec.Title(kind, id)
	if t == nil {
		writeError(w, http.StatusNotFound, rsync.ErrNotFound)
		return
	}
	resp := titleResponse{Title: t}
	if t.Partial {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		nt, err := sec.SyncTitle(ctx, kind, id, false, false)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Title = nt
		}
	}
	b, _ := json.Marshal(resp)
	writeJSON(w, http.StatusOK, b)
}

// syncTitle syncs one title and returns it. The sync goes on if the client
// gives up waiting.
func (s *Server) syncTitle(w http.ResponseWriter, r *http.Request) {
	sec := s.section(w, r)
	if sec == nil {
		return
	}
	q := r.URL.Query()
	t, err := sec.SyncTitle(r.Context(), q.Get("kind"), q.Get("id"), flag(r, "deep"), true)
	switch {
	case errors.Is(err, rsync.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, rsync.ErrBusy):
		writeError(w, http.StatusConflict, err)
	case t == nil:
		writeError(w, http.StatusBadGateway, err)
	default:
		resp := titleResponse{Title: t}
		if err != nil {
			resp.Error = err.Error() // synced on some sources, not all
		}
		b, _ := json.Marshal(resp)
		writeJSON(w, http.StatusOK, b)
	}
}

func writeError(w http.ResponseWriter, code int, err error) {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	writeJSON(w, code, b)
}

func writeJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(body)
}
