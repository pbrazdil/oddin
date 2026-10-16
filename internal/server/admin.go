package server

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"
	"time"

	"oddsboard/internal/board"
	"oddsboard/internal/feed"
	"oddsboard/internal/market"
)

func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"simulator":   s.opt.Simulator != nil,
		"journalHead": s.opt.Board.JournalHead(),
	})
}

func (s *Server) knownMarket(w http.ResponseWriter, r *http.Request) (market.Key, bool) {
	key := keyOf(r)
	if _, ok := s.opt.Board.Current(key); !ok {
		writeError(w, http.StatusNotFound, "unknown market")
		return key, false
	}
	return key, true
}

// adminHistory pages through a market's journal: every message received,
// the verdict, a plain-language summary and the state shown afterwards.
func (s *Server) adminHistory(w http.ResponseWriter, r *http.Request) {
	key, ok := s.knownMarket(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 || limit > 5000 {
		limit = 1000
	}
	recs := s.opt.Board.History(key, after, limit)
	writeJSON(w, http.StatusOK, map[string]any{"records": recs, "journalHead": s.opt.Board.JournalHead()})
}

// adminStateAt answers "what did the board show at time t?", e.g. the
// moment a customer placed a bet.
func (s *Server) adminStateAt(w http.ResponseWriter, r *http.Request) {
	key, ok := s.knownMarket(w, r)
	if !ok {
		return
	}
	t, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("t"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "t must be an RFC 3339 timestamp")
		return
	}
	rec, ok := s.opt.Board.StateAt(key, t)
	if !ok {
		writeError(w, http.StatusNotFound, "no state recorded at or before that time")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record": rec})
}

func (s *Server) adminReset(w http.ResponseWriter, r *http.Request) {
	key, ok := s.knownMarket(w, r)
	if !ok {
		return
	}
	p, err := s.opt.Board.Reset(key, board.SourceAdmin)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"epoch": p.Epoch, "version": p.Version})
}

type replayRequest struct {
	Events        jsontext.Value `json:"events"` // a JSON array of events (or one event), kept byte-exact
	IntervalMs    int64          `json:"intervalMs"`
	Reset         bool           `json:"reset"`         // reset the market first
	StopSimulator bool           `json:"stopSimulator"` // so the simulator does not interleave
}

// adminReplay feeds a list of events into the live board, optionally after
// a reset, paced like the real feed. It is how testers replay a scenario.
func (s *Server) adminReplay(w http.ResponseWriter, r *http.Request) {
	key, ok := s.knownMarket(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req replayRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid replay request: "+err.Error())
		return
	}
	events, err := market.SplitBatch(req.Events)
	if err != nil {
		writeError(w, http.StatusBadRequest, "events: "+err.Error())
		return
	}
	if len(events) == 0 || len(events) > board.MaxBatch {
		writeError(w, http.StatusBadRequest, "events must hold 1 to 1000 messages")
		return
	}
	if req.IntervalMs < 0 || req.IntervalMs > 60_000 {
		writeError(w, http.StatusBadRequest, "intervalMs must be between 0 and 60000")
		return
	}
	if s.opt.Replayer.Status().Running {
		writeError(w, http.StatusConflict, feed.ErrReplayRunning.Error())
		return
	}
	if req.StopSimulator && s.opt.Simulator != nil {
		cfg, _ := s.opt.Simulator.Status()
		cfg.Running = false
		s.opt.Simulator.Configure(cfg)
	}
	if req.Reset {
		if _, err := s.opt.Board.Reset(key, board.SourceReplay); err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	if err := s.opt.Replayer.Start(events, time.Duration(req.IntervalMs)*time.Millisecond); err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, feed.ErrReplayRunning) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.opt.Replayer.Status())
}

func (s *Server) adminReplayStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opt.Replayer.Status())
}

func (s *Server) adminReplayStop(w http.ResponseWriter, r *http.Request) {
	s.opt.Replayer.Stop()
	writeJSON(w, http.StatusOK, s.opt.Replayer.Status())
}

func (s *Server) simulator(w http.ResponseWriter) bool {
	if s.opt.Simulator == nil {
		writeError(w, http.StatusNotFound, "simulator disabled")
		return false
	}
	return true
}

func (s *Server) writeSimulator(w http.ResponseWriter) {
	settings, stats := s.opt.Simulator.Status()
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "stats": stats})
}

func (s *Server) adminSimulator(w http.ResponseWriter, r *http.Request) {
	if s.simulator(w) {
		s.writeSimulator(w)
	}
}

func (s *Server) adminSimulatorConfigure(w http.ResponseWriter, r *http.Request) {
	if !s.simulator(w) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var cfg feed.Settings
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}
	s.opt.Simulator.Configure(cfg)
	s.writeSimulator(w)
}

func (s *Server) adminSimulatorInterrupt(w http.ResponseWriter, r *http.Request) {
	if !s.simulator(w) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Seconds float64 `json:"seconds"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Seconds < 0 || req.Seconds > 600 {
		writeError(w, http.StatusBadRequest, "seconds must be between 0 and 600")
		return
	}
	s.opt.Simulator.Interrupt(time.Duration(req.Seconds * float64(time.Second)))
	s.writeSimulator(w)
}

func (s *Server) adminVerify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opt.Board.Verify())
}
