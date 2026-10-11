package server

import "net/http"

// config serves GET /config. It is a placeholder that returns an empty JSON
// object; fill in whatever the site needs.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []byte("{}\n"))
}
