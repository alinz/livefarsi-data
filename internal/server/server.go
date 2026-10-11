// Package server is the HTTP API and website.
//
//	GET /               the website (web/index.html)
//	GET /rsync/start    start a sync (?deep=1 re-checks every title); 202, or 409 if one is running
//	GET /rsync/status   the current or last sync's status
//	GET /rsync/data     the merged catalog as JSON, gzip-compressed
//	GET /config         site configuration (see config.go)
package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"strings"

	"ella.to/movies/internal/rsync"
	"ella.to/movies/web"
)

type Server struct {
	sync *rsync.Syncer
}

func New(s *rsync.Syncer) http.Handler {
	srv := &Server{sync: s}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.index)
	mux.HandleFunc("GET /rsync/start", srv.start)
	mux.HandleFunc("GET /rsync/status", srv.status)
	mux.HandleFunc("GET /rsync/data", srv.data)
	mux.HandleFunc("GET /config", srv.config)
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(web.Index)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	deep := r.URL.Query().Get("deep")
	code := http.StatusAccepted
	if !s.sync.Start(deep == "1" || deep == "true") {
		code = http.StatusConflict // already running
	}
	writeJSON(w, code, s.sync.Status())
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sync.Status())
}

// data serves the merged catalog. It is kept gzipped in memory; the rare
// client that doesn't accept gzip gets it decompressed on the fly.
func (s *Server) data(w http.ResponseWriter, r *http.Request) {
	d := s.sync.Data()
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

func writeJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(body)
}
