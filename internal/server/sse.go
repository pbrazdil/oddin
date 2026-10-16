package server

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"time"
)

// stream serves a market as Server-Sent Events.
//
// Every "state" event carries the complete market state, never a delta,
// so a client is always exactly one message away from the truth: after a
// reconnect the first event it receives is the current state. "heartbeat"
// events carry the server's current version and clock so the client can
// detect both a silent connection and a state it missed.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	key := keyOf(r)
	notify, cancel, ok := s.opt.Board.Subscribe(key)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown market")
		return
	}
	defer cancel()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)

	write := func(chunk string) bool {
		// A client that cannot take a few hundred bytes in 10s is gone.
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprint(w, chunk); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	var epoch string
	var version uint64
	send := func() bool {
		p, _ := s.opt.Board.Current(key)
		if p.Epoch == epoch && p.Version == version {
			return true
		}
		epoch, version = p.Epoch, p.Version
		return write(fmt.Sprintf("event: state\nid: %s\ndata: %s\n\n", stateTag(p), p.JSON))
	}

	if !write("retry: 2000\n\n") || !send() {
		return
	}
	hb := time.NewTicker(s.opt.Heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.opt.Done:
			return
		case <-notify:
			if !send() {
				return
			}
		case <-hb.C:
			p, _ := s.opt.Board.Current(key)
			beat, _ := json.Marshal(struct {
				Epoch      string    `json:"epoch"`
				Version    uint64    `json:"version"`
				ServerTime time.Time `json:"serverTime"`
			}{p.Epoch, p.Version, time.Now().UTC()})
			if !write(fmt.Sprintf("event: heartbeat\ndata: %s\n\n", beat)) {
				return
			}
		}
	}
}
