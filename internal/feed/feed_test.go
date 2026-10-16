package feed

import (
	"encoding/json/jsontext"
	"sync"
	"testing"
	"time"

	"oddsboard/internal/board"
	"oddsboard/internal/market"
)

var def = market.Definition{
	Key: market.Key{MatchID: "match-1", MarketID: "winner"}, Event: "Falcons vs Wolves", Name: "Match winner",
	Outcomes: []market.OutcomeDef{{ID: "falcons", Name: "Falcons"}, {ID: "wolves", Name: "Wolves"}},
}

type sink struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (s *sink) deliver(b []byte) {
	s.mu.Lock()
	s.msgs = append(s.msgs, b)
	s.mu.Unlock()
}

// TestSimulatorSnapshotMatchesItsFeed checks that the provider's snapshot is
// exactly what folding its own messages in order gives. Recovery relies on it.
func TestSimulatorSnapshotMatchesItsFeed(t *testing.T) {
	for seed := range uint64(20) {
		s := &sink{}
		sim := NewSimulator(def, seed, s.deliver)
		sim.Configure(Settings{Running: true, IntervalMs: 1000})
		m := market.New(def, market.Config{StaleAfter: time.Hour})
		at := time.Now()
		for range 300 {
			sim.Step()
		}
		for _, raw := range s.msgs {
			ev, err := market.ParseEvent(raw)
			if err != nil {
				t.Fatalf("seed %d: simulator produced invalid message %s: %v", seed, raw, err)
			}
			if r := m.ApplyEvent(ev, at); r.Verdict != market.VerdictApplied {
				t.Fatalf("seed %d: %s -> %s", seed, raw, r.Verdict)
			}
		}
		raw, err := sim.Snapshot(t.Context(), def.Key)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := market.ParseSnapshot(raw)
		if err != nil {
			t.Fatalf("invalid snapshot %s: %v", raw, err)
		}
		st := m.State(at)
		if !st.Consistent || snap.Seq != st.Seq || snap.Status != st.Status {
			t.Fatalf("seed %d: snapshot %s vs state %+v", seed, raw, st)
		}
		for o, p := range snap.Odds {
			if got, _ := st.Price(o); got != p {
				t.Fatalf("seed %d: %s snapshot %s, fold %s", seed, o, p, got)
			}
		}
		// Applying the snapshot to the folded market must agree with it.
		if r := m.ApplySnapshot(snap, at); r.Verdict != market.VerdictApplied || len(r.Changes) != 0 {
			t.Fatalf("seed %d: snapshot disagreed with the feed: %+v", seed, r)
		}
	}
}

func TestSimulatorProducesSuspensionsAndPartialUpdates(t *testing.T) {
	s := &sink{}
	sim := NewSimulator(def, 1, s.deliver)
	sim.Configure(Settings{Running: true})
	for range 500 {
		sim.Step()
	}
	var suspended, partial int
	for _, raw := range s.msgs {
		ev, _ := market.ParseEvent(raw)
		if ev.Status == market.StatusSuspended {
			suspended++
		}
		if len(ev.Odds) == 1 {
			partial++
		}
	}
	if suspended == 0 || partial == 0 {
		t.Fatalf("suspensions %d, single-outcome updates %d", suspended, partial)
	}
	first, _ := market.ParseEvent(s.msgs[0])
	if first.Seq != 41 || first.Status != market.StatusOpen || len(first.Odds) != 2 {
		t.Fatalf("first message %s", s.msgs[0])
	}
}

func TestSimulatorChaos(t *testing.T) {
	s := &sink{}
	sim := NewSimulator(def, 3, s.deliver)
	sim.Configure(Settings{Running: true, DropRate: 0.2, DuplicateRate: 0.2, ReorderRate: 0.2})
	for range 500 {
		sim.Step()
	}
	_, st := sim.Status()
	if st.Generated != 500 || st.Dropped == 0 || st.Duplicated == 0 || st.Reordered == 0 {
		t.Fatalf("stats %+v", st)
	}
	if uint64(len(s.msgs)) != st.Delivered {
		t.Fatalf("delivered %d, counted %d", len(s.msgs), st.Delivered)
	}
}

func TestSimulatorInterruptAndStop(t *testing.T) {
	s := &sink{}
	sim := NewSimulator(def, 4, s.deliver)
	sim.Step()
	if len(s.msgs) != 0 {
		t.Fatal("stopped simulator delivered")
	}
	sim.Configure(Settings{Running: true})
	sim.Interrupt(time.Hour)
	for range 5 {
		sim.Step()
	}
	_, st := sim.Status()
	if len(s.msgs) != 0 || st.Dropped != 5 || st.Seq != 45 {
		t.Fatalf("interrupted: %d delivered, stats %+v", len(s.msgs), st)
	}
	sim.Interrupt(0)
	sim.Step()
	if len(s.msgs) != 1 {
		t.Fatal("did not resume after the interruption")
	}
	if ev, _ := market.ParseEvent(s.msgs[0]); ev.Seq != 46 {
		t.Fatalf("resumed at seq %d", ev.Seq)
	}
}

func TestReplayer(t *testing.T) {
	var mu sync.Mutex
	var got []string
	r := NewReplayer(func(b []byte) (board.Ack, error) {
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		return board.Ack{Verdict: market.VerdictApplied}, nil
	})
	items := []jsontext.Value{jsontext.Value(`{"a":1}`), jsontext.Value(`{"b":2}`), jsontext.Value(`{"c":3}`)}

	if err := r.Start(items, 0); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Running || st.Sent != 3 || len(st.Acks) != 3 {
		t.Fatalf("immediate replay: %+v", st)
	}

	if err := r.Start(items, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(items, time.Hour); err != ErrReplayRunning {
		t.Fatalf("second start: %v", err)
	}
	r.Stop()
	st := r.Status()
	if st.Running || st.Sent != 1 || st.Error != "stopped" {
		t.Fatalf("stopped replay: %+v", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("submitted %d", len(got))
	}
}

func TestSimulatorContinueAfter(t *testing.T) {
	s := &sink{}
	sim := NewSimulator(def, 5, s.deliver)
	sim.Configure(Settings{Running: true})
	sim.ContinueAfter(500)
	sim.Step()
	ev, err := market.ParseEvent(s.msgs[0])
	if err != nil || ev.Seq != 501 || ev.Status == "" || len(ev.Odds) != 2 {
		t.Fatalf("first message after ContinueAfter: %s (%v)", s.msgs[0], err)
	}
}
