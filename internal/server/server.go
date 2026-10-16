// Package server exposes the board over HTTP: the public state and SSE
// stream, the authenticated feed ingestion endpoints, the superadmin API
// and the embeddable frontend.
package server

import (
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"oddsboard/internal/board"
	"oddsboard/internal/feed"
	"oddsboard/internal/market"
)

// Options configure a Server.
type Options struct {
	Board      *board.Board
	Simulator  *feed.Simulator // nil when the simulator is disabled
	Replayer   *feed.Replayer
	Assets     fs.FS // built frontend
	AdminToken string
	FeedToken  string
	// FrameAncestors is the CSP frame-ancestors source list for the embed
	// page, e.g. "*" or "https://partner.example https://*.partner.example".
	FrameAncestors string
	Heartbeat      time.Duration
	Log            *slog.Logger
	// Done closes open event streams on shutdown.
	Done <-chan struct{}
}

// Server is an http.Handler.
type Server struct {
	opt Options
	mux *http.ServeMux
}

const maxBody = 1 << 20

// New builds the handler.
func New(opt Options) *Server {
	if opt.Heartbeat == 0 {
		opt.Heartbeat = time.Second
	}
	if opt.FrameAncestors == "" {
		opt.FrameAncestors = "*"
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	s := &Server{opt: opt, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.health)

	// Public, read-only and CORS-enabled so plain JS on any site can use it.
	m.HandleFunc("GET /api/v1/markets", public(s.listMarkets))
	m.HandleFunc("GET /api/v1/markets/{matchId}/{marketId}", public(s.marketState))
	m.HandleFunc("GET /api/v1/markets/{matchId}/{marketId}/stream", public(s.stream))

	// Feed ingestion: feed or admin token.
	m.HandleFunc("POST /api/v1/feed/events", s.feedAuth(s.postEvents))
	m.HandleFunc("POST /api/v1/feed/snapshot", s.feedAuth(s.postSnapshot))

	// Superadmin: admin token only, same-origin (no CORS).
	m.HandleFunc("GET /api/v1/admin/session", s.adminAuth(s.adminSession))
	m.HandleFunc("GET /api/v1/admin/markets/{matchId}/{marketId}/history", s.adminAuth(s.adminHistory))
	m.HandleFunc("GET /api/v1/admin/markets/{matchId}/{marketId}/at", s.adminAuth(s.adminStateAt))
	m.HandleFunc("POST /api/v1/admin/markets/{matchId}/{marketId}/reset", s.adminAuth(s.adminReset))
	m.HandleFunc("POST /api/v1/admin/markets/{matchId}/{marketId}/replay", s.adminAuth(s.adminReplay))
	m.HandleFunc("GET /api/v1/admin/replay", s.adminAuth(s.adminReplayStatus))
	m.HandleFunc("DELETE /api/v1/admin/replay", s.adminAuth(s.adminReplayStop))
	m.HandleFunc("GET /api/v1/admin/simulator", s.adminAuth(s.adminSimulator))
	m.HandleFunc("PUT /api/v1/admin/simulator", s.adminAuth(s.adminSimulatorConfigure))
	m.HandleFunc("POST /api/v1/admin/simulator/interrupt", s.adminAuth(s.adminSimulatorInterrupt))
	m.HandleFunc("GET /api/v1/admin/verify", s.adminAuth(s.adminVerify))

	// Frontend.
	m.HandleFunc("GET /embed", s.embedDefault)
	m.HandleFunc("GET /embed/{matchId}/{marketId}", s.embedPage)
	m.HandleFunc("GET /embed.js", s.partnerScript("embed.js"))
	m.HandleFunc("GET /client.js", s.partnerScript("client.js"))
	m.HandleFunc("GET /assets/{file...}", s.asset)
	m.HandleFunc("GET /demo/{$}", s.demoPage)
	m.HandleFunc("GET /demo/{file}", s.demoFile)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/demo/", http.StatusFound)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
	}
	start := time.Now()
	rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rw, r)
	if !strings.HasSuffix(r.URL.Path, "/stream") || rw.status != http.StatusOK {
		s.opt.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "dur", time.Since(start))
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush and SetWriteDeadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func public(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h(w, r)
	}
}

func bearer(r *http.Request) string {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

func tokenIs(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) feedAuth(h func(http.ResponseWriter, *http.Request, board.Source)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		switch {
		case tokenIs(tok, s.opt.AdminToken):
			h(w, r, board.SourceAdmin)
		case tokenIs(tok, s.opt.FeedToken):
			h(w, r, board.SourceFeed)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="feed"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		}
	}
}

func (s *Server) adminAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenIs(bearer(r), s.opt.AdminToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid admin token")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, v); err != nil {
		slog.Error("encoding response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body exceeds 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "reading body: "+err.Error())
		}
		return nil, false
	}
	return body, true
}

func keyOf(r *http.Request) market.Key {
	return market.Key{MatchID: r.PathValue("matchId"), MarketID: r.PathValue("marketId")}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "journalHead": s.opt.Board.JournalHead()})
}

type marketInfo struct {
	MatchID  string              `json:"matchId"`
	MarketID string              `json:"marketId"`
	Event    string              `json:"event"`
	Market   string              `json:"market"`
	Outcomes []market.OutcomeDef `json:"outcomes"`
	Embed    string              `json:"embed"`
	Stream   string              `json:"stream"`
}

func (s *Server) listMarkets(w http.ResponseWriter, r *http.Request) {
	out := []marketInfo{}
	for _, d := range s.opt.Board.Markets() {
		path := "/" + d.Key.MatchID + "/" + d.Key.MarketID
		out = append(out, marketInfo{
			MatchID: d.Key.MatchID, MarketID: d.Key.MarketID, Event: d.Event, Market: d.Name, Outcomes: d.Outcomes,
			Embed: "/embed" + path, Stream: "/api/v1/markets" + path + "/stream",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"markets": out})
}

// marketState returns the current state. With ?after=<epoch>.<version>&wait=<ms>
// it long-polls: it answers as soon as the state differs from the one the
// client holds, or after wait (at most 5 s) with the unchanged state. This is
// the fallback for networks that buffer event streams (some proxies, and
// Cloudflare quick tunnels, which do not support SSE). X-Server-Time lets the
// client measure feed age on the server's clock, as heartbeats do.
func (s *Server) marketState(w http.ResponseWriter, r *http.Request) {
	key := keyOf(r)
	q := r.URL.Query()
	wait, _ := strconv.Atoi(q.Get("wait"))
	wait = min(max(wait, 0), 5000)
	after := q.Get("after")

	var notify <-chan struct{}
	if wait > 0 && after != "" {
		// Subscribe before reading the state so no change can slip between.
		ch, cancel, ok := s.opt.Board.Subscribe(key)
		if ok {
			defer cancel()
			notify = ch
		}
	}
	p, ok := s.opt.Board.Current(key)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown market")
		return
	}
	if notify != nil && after == stateTag(p) {
		t := time.NewTimer(time.Duration(wait) * time.Millisecond)
		defer t.Stop()
		select {
		case <-notify:
		case <-t.C:
		case <-s.opt.Done:
		case <-r.Context().Done():
			return
		}
		p, _ = s.opt.Board.Current(key)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Server-Time", time.Now().UTC().Format(time.RFC3339Nano))
	h.Set("Access-Control-Expose-Headers", "X-Server-Time")
	w.Write(p.JSON)
}

// stateTag identifies a published state, as in SSE event ids.
func stateTag(p *board.Published) string { return p.Epoch + "." + strconv.FormatUint(p.Version, 10) }

func (s *Server) postEvents(w http.ResponseWriter, r *http.Request, src board.Source) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	acks, err := s.opt.Board.SubmitBatch(body, src)
	switch {
	case errors.Is(err, board.ErrUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "acks": acks})
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"acks": acks})
	}
}

func (s *Server) postSnapshot(w http.ResponseWriter, r *http.Request, src board.Source) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	ack, err := s.opt.Board.SubmitSnapshot(body, src)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ack": ack})
}
