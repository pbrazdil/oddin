// Package feed provides the things that put messages into the board: a
// provider simulator for demos and tests, a paced replayer, and an HTTP
// client for pushing recorded feeds into a running server.
package feed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"oddsboard/internal/market"
)

// Settings control the simulator. Rates are probabilities in [0, 1].
type Settings struct {
	Running       bool      `json:"running"`
	IntervalMs    int       `json:"intervalMs"`
	DropRate      float64   `json:"dropRate"`      // messages lost in transit
	DuplicateRate float64   `json:"duplicateRate"` // messages delivered twice
	ReorderRate   float64   `json:"reorderRate"`   // messages held back and delivered after the next one
	InterruptedTo time.Time `json:"interruptedUntil,omitzero"`
}

// Stats count what the simulator did.
type Stats struct {
	Seq        uint64 `json:"seq"` // provider's current seq
	Generated  uint64 `json:"generated"`
	Delivered  uint64 `json:"delivered"`
	Dropped    uint64 `json:"dropped"`
	Duplicated uint64 `json:"duplicated"`
	Reordered  uint64 `json:"reordered"`
}

// Simulator plays a feed provider for one market: it generates a plausible
// stream of partial updates about once per interval and delivers them
// through an unreliable channel. It also serves snapshots of its true
// state, like a provider's recovery endpoint would.
type Simulator struct {
	mu       sync.Mutex
	def      market.Definition
	deliver  func([]byte)
	rng      *rand.Rand
	settings Settings
	stats    Stats
	now      func() time.Time

	status    market.Status
	prob      float64 // implied probability of the first outcome winning
	odds      map[market.Outcome]market.Price
	suspended int // ticks left in the current suspension
	held      []byte
}

const margin = 1.06 // bookmaker overround

// NewSimulator returns a stopped simulator for a two-outcome market.
// deliver is called without the simulator's lock held.
func NewSimulator(def market.Definition, seed uint64, deliver func([]byte)) *Simulator {
	if len(def.Outcomes) != 2 {
		panic("simulator supports two-outcome markets only")
	}
	s := &Simulator{
		def:      def,
		deliver:  deliver,
		rng:      rand.New(rand.NewPCG(seed, seed^0x5eed)),
		settings: Settings{IntervalMs: 1000},
		now:      time.Now,
		prob:     0.52,
		odds:     map[market.Outcome]market.Price{},
	}
	s.stats.Seq = 40 // so the first message matches the brief's example
	return s
}

// Run generates messages until ctx ends.
func (s *Simulator) Run(ctx context.Context) {
	for {
		s.mu.Lock()
		interval := time.Duration(s.settings.IntervalMs) * time.Millisecond
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		s.Step()
	}
}

// Step generates one message if running and delivers whatever the
// unreliable channel lets through.
func (s *Simulator) Step() {
	s.mu.Lock()
	if !s.settings.Running {
		s.mu.Unlock()
		return
	}
	msg := s.generateLocked()
	out := s.transmitLocked(msg)
	s.mu.Unlock()
	for _, m := range out {
		s.deliver(m)
	}
}

func (s *Simulator) generateLocked() []byte {
	s.stats.Seq++
	s.stats.Generated++
	ev := market.Event{Key: s.def.Key, Seq: s.stats.Seq}
	a, b := s.def.Outcomes[0].ID, s.def.Outcomes[1].ID

	switch {
	case s.status == "":
		// First message: everything, like seq 41 in the brief.
		s.status = market.StatusOpen
		s.reprice(a, b)
		ev.Status, ev.Odds = s.status, map[market.Outcome]market.Price{a: s.odds[a], b: s.odds[b]}
	case s.suspended > 0:
		s.suspended--
		if s.suspended == 0 {
			s.status = market.StatusOpen
			ev.Status = s.status
			if s.rng.IntN(2) == 0 { // reopen at new prices
				s.move(0.06)
				s.reprice(a, b)
				ev.Odds = map[market.Outcome]market.Price{a: s.odds[a], b: s.odds[b]}
			}
		} else {
			// Prices may keep moving while suspended.
			s.move(0.03)
			s.reprice(a, b)
			ev.Odds = map[market.Outcome]market.Price{a: s.odds[a], b: s.odds[b]}
		}
	case s.rng.Float64() < 0.06:
		s.status = market.StatusSuspended
		s.suspended = 2 + s.rng.IntN(4)
		ev.Status = s.status
	default:
		old := map[market.Outcome]market.Price{a: s.odds[a], b: s.odds[b]}
		s.move(0.025)
		s.reprice(a, b)
		ev.Odds = map[market.Outcome]market.Price{}
		for _, o := range []market.Outcome{a, b} {
			if s.odds[o] != old[o] {
				ev.Odds[o] = s.odds[o]
			}
		}
		// Sometimes the provider only republishes one side.
		if len(ev.Odds) == 2 && s.rng.IntN(3) == 0 {
			keep := []market.Outcome{a, b}[s.rng.IntN(2)]
			for _, o := range []market.Outcome{a, b} {
				if o != keep {
					delete(ev.Odds, o)
					s.odds[o] = old[o]
				}
			}
		}
	}
	return market.EncodeEvent(ev)
}

// move nudges the implied probability by up to ±step.
func (s *Simulator) move(step float64) {
	s.prob += (s.rng.Float64()*2 - 1) * step
	s.prob = min(0.85, max(0.15, s.prob))
}

func (s *Simulator) reprice(a, b market.Outcome) {
	s.odds[a] = decimalOdds(s.prob)
	s.odds[b] = decimalOdds(1 - s.prob)
}

// decimalOdds converts a probability to two-decimal odds with margin.
func decimalOdds(p float64) market.Price {
	o := math.Round(100/(p*margin)) / 100
	return market.Price(max(int64(o*100+0.5)*100, int64(market.MinPrice)+99))
}

// transmitLocked applies the unreliable channel to one generated message.
func (s *Simulator) transmitLocked(msg []byte) [][]byte {
	if s.now().Before(s.settings.InterruptedTo) || s.rng.Float64() < s.settings.DropRate {
		s.stats.Dropped++
		return nil
	}
	var out [][]byte
	if s.held == nil && s.rng.Float64() < s.settings.ReorderRate {
		s.held = msg
		s.stats.Reordered++
		return nil
	}
	out = append(out, msg)
	if s.rng.Float64() < s.settings.DuplicateRate {
		out = append(out, msg)
		s.stats.Duplicated++
	}
	if s.held != nil {
		out = append(out, s.held)
		s.held = nil
	}
	s.stats.Delivered += uint64(len(out))
	return out
}

// Snapshot returns the provider's true current state. It implements
// board.SnapshotSource.
func (s *Simulator) Snapshot(_ context.Context, key market.Key) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != s.def.Key {
		return nil, fmt.Errorf("simulator does not serve %s", key)
	}
	if s.status == "" {
		return nil, errors.New("simulator has not started this market yet")
	}
	return market.EncodeSnapshot(market.Snapshot{
		Key: s.def.Key, Seq: s.stats.Seq, Status: s.status, Odds: map[market.Outcome]market.Price{
			s.def.Outcomes[0].ID: s.odds[s.def.Outcomes[0].ID],
			s.def.Outcomes[1].ID: s.odds[s.def.Outcomes[1].ID],
		},
	}), nil
}

// Configure replaces the settings, keeping a running interruption.
func (s *Simulator) Configure(next Settings) Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	next.IntervalMs = min(10_000, max(100, next.IntervalMs))
	next.DropRate = clamp01(next.DropRate)
	next.DuplicateRate = clamp01(next.DuplicateRate)
	next.ReorderRate = clamp01(next.ReorderRate)
	next.InterruptedTo = s.settings.InterruptedTo
	s.settings = next
	return s.settings
}

// Interrupt drops every message for d, simulating a broken connection to
// the provider. The provider keeps generating, so the board sees a gap.
func (s *Simulator) Interrupt(d time.Duration) Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.InterruptedTo = s.now().Add(d)
	return s.settings
}

// Restart forgets the provider state so the next message starts fresh.
func (s *Simulator) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.suspended, s.held = "", 0, nil
	s.stats = Stats{Seq: 40}
}

// ContinueAfter makes the next message use seq+1 (at least 41) and carry
// the full state, as a provider resuming its stream would.
func (s *Simulator) ContinueAfter(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Seq = max(s.stats.Seq, seq)
	s.status = ""
}

// Status returns the current settings and counters.
func (s *Simulator) Status() (Settings, Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings, s.stats
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return min(1, max(0, x))
}
