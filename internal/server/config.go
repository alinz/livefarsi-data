package server

import (
	"net/http"

	"ella.to/baker"
)

// domain is the public host baker routes to this service.
const domain = "farsi.ella.to"

// routes is the baker routing config, built once since it never changes.
var routes = baker.NewEntryList().
	New(domain, "/*", true).
	CacheResponse()

// config serves GET /config: baker pings it (baker.service.ping in
// deploy/server/docker-compose.yml) to learn which domain and paths to proxy
// here.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	routes.WriteResponse(w)
}
