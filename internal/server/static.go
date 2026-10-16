package server

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// The embed page must be framable by partner sites, so it sends no
// X-Frame-Options and controls framing with CSP frame-ancestors instead.
// Everything else is locked down: scripts and connections only to this
// origin, no inline script.
func (s *Server) embedCSP() string {
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; frame-ancestors " + s.opt.FrameAncestors + "; base-uri 'none'; form-action 'none'; object-src 'none'"
}

func (s *Server) embedDefault(w http.ResponseWriter, r *http.Request) {
	defs := s.opt.Board.Markets()
	if len(defs) == 0 {
		http.NotFound(w, r)
		return
	}
	target := "/embed/" + defs[0].Key.MatchID + "/" + defs[0].Key.MarketID
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) embedPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.opt.Board.Current(keyOf(r)); !ok {
		writeError(w, http.StatusNotFound, "unknown market")
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", s.embedCSP())
	// Allow embedding by pages that require CORP (COEP: require-corp).
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Cache-Control", "no-cache")
	s.serveFile(w, r, "index.html")
}

// partnerScript serves the scripts partner pages load from their own
// origin: the embed.js loader and the client.js stream module (module
// scripts need CORS).
func (s *Server) partnerScript(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Cache-Control", "public, max-age=300")
		s.serveFile(w, r, name)
	}
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	// Bundles have content hashes in their names.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	s.serveFile(w, r, path.Join("assets", r.PathValue("file")))
}

// The demo pages play the part of a partner site embedding the board.
func (s *Server) demoCSP() string {
	return "default-src 'self'; script-src 'self'; style-src 'self'; frame-src 'self'; connect-src 'self'; base-uri 'none'; object-src 'none'"
}

func (s *Server) demoPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", s.demoCSP())
	w.Header().Set("Cache-Control", "no-cache")
	s.serveFile(w, r, "demo/index.html")
}

func (s *Server) demoFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", s.demoCSP())
	w.Header().Set("Cache-Control", "no-cache")
	s.serveFile(w, r, path.Join("demo", r.PathValue("file")))
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if s.opt.Assets == nil || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.opt.Assets, name)
	if err != nil {
		if name == "index.html" {
			http.Error(w, "frontend not built: run `make web`", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Write(data)
}
