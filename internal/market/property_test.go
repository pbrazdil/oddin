package market

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// The tests in this file check the merge against an independent oracle on
// randomly generated feeds with loss, duplication, reordering and
// snapshots. The ground truth is a single in-order feed; the oracle folds
// it the obvious way.

type truth struct {
	base   uint64
	events []Event // seqs base, base+1, ...; the first one is complete
}

var outcomes = []Outcome{"falcons", "wolves"}

func genTruth(rng *rand.Rand, n int) truth {
	tr := truth{base: 1 + rng.Uint64N(100)}
	randPrice := func() Price { return Price(10_100 + rng.IntN(200)*500) } // 1.01 .. 10.96
	for i := range n {
		e := Event{Key: testKey, Seq: tr.base + uint64(i)}
		full := i == 0
		if full || rng.IntN(3) == 0 {
			e.Status = []Status{StatusOpen, StatusOpen, StatusSuspended, StatusClosed}[rng.IntN(4)]
		}
		for _, o := range outcomes {
			if full || rng.IntN(2) == 0 {
				if e.Odds == nil {
					e.Odds = map[Outcome]Price{}
				}
				e.Odds[o] = randPrice()
			}
		}
		tr.events = append(tr.events, e)
	}
	return tr
}

func (tr truth) last() uint64 { return tr.base + uint64(len(tr.events)) - 1 }

// at folds every event up to and including seq, in order.
func (tr truth) at(seq uint64) (Status, map[Outcome]Price) {
	st, odds := Status(""), map[Outcome]Price{}
	for _, e := range tr.events {
		if e.Seq > seq {
			break
		}
		if e.Status != "" {
			st = e.Status
		}
		for o, p := range e.Odds {
			odds[o] = p
		}
	}
	return st, odds
}

func (tr truth) snapshot(seq uint64) Snapshot {
	st, odds := tr.at(seq)
	return Snapshot{Key: testKey, Seq: seq, Status: st, Odds: odds}
}

// delivery is one message as the board receives it.
type delivery struct {
	event    *Event
	snapshot *Snapshot
}

func (d delivery) seq() uint64 {
	if d.event != nil {
		return d.event.Seq
	}
	return d.snapshot.Seq
}

type chaos struct {
	drop, dup, snap float64 // probabilities per truth event
	shuffle         int     // 0 = in order, -1 = full shuffle, k = local swaps within k
}

func genDeliveries(rng *rand.Rand, tr truth, c chaos) []delivery {
	var out []delivery
	for i := range tr.events {
		e := tr.events[i]
		if rng.Float64() >= c.drop {
			out = append(out, delivery{event: &e})
			if rng.Float64() < c.dup {
				out = append(out, delivery{event: &e})
			}
		}
		if rng.Float64() < c.snap {
			s := tr.snapshot(tr.base + rng.Uint64N(uint64(len(tr.events))))
			out = append(out, delivery{snapshot: &s})
		}
	}
	switch {
	case c.shuffle < 0:
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	case c.shuffle > 0:
		for i := range out {
			j := min(len(out)-1, i+rng.IntN(c.shuffle+1))
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// lww is the oracle for the merge rule: each field takes the value from the
// highest-seq delivered message that states it.
type lww struct {
	status    Status
	statusSeq uint64
	prices    map[Outcome]Price
	seqs      map[Outcome]uint64
}

func (l *lww) observe(d delivery) {
	seq, st, odds := d.seq(), Status(""), map[Outcome]Price(nil)
	if d.event != nil {
		st, odds = d.event.Status, d.event.Odds
	} else {
		st, odds = d.snapshot.Status, d.snapshot.Odds
	}
	if st != "" && seq > l.statusSeq {
		l.status, l.statusSeq = st, seq
	}
	for o, p := range odds {
		if seq > l.seqs[o] {
			l.prices[o], l.seqs[o] = p, seq
		}
	}
}

func runTrial(t *testing.T, rng *rand.Rand, n int, c chaos) {
	t.Helper()
	tr := genTruth(rng, n)
	deliveries := genDeliveries(rng, tr, c)
	m := New(testDef, Config{StaleAfter: time.Hour})
	oracle := lww{prices: map[Outcome]Price{}, seqs: map[Outcome]uint64{}}

	received := map[uint64]bool{} // event seqs delivered so far
	var snapCover, maxSeen uint64 // highest snapshot seq; highest seq of any delivery

	for i, d := range deliveries {
		at := t0.Add(time.Duration(i) * time.Millisecond)
		before := mustJSON(t, m.State(at))
		var r Result
		if d.event != nil {
			r = m.ApplyEvent(*d.event, at)
		} else {
			r = m.ApplySnapshot(*d.snapshot, at)
		}
		where := fmt.Sprintf("delivery %d/%d (seq %d, snapshot=%v)", i, len(deliveries), d.seq(), d.snapshot != nil)

		// Verdicts follow from what was delivered before.
		switch {
		case d.event != nil && received[d.seq()]:
			if r.Verdict != VerdictDuplicate {
				t.Fatalf("%s: re-delivery verdict %s", where, r.Verdict)
			}
			if mustJSON(t, m.State(at)) != before {
				t.Fatalf("%s: duplicate changed the state", where)
			}
		case d.seq() > maxSeen:
			if r.Verdict != VerdictApplied || !r.Progress {
				t.Fatalf("%s: new head verdict %s progress %v", where, r.Verdict, r.Progress)
			}
		default:
			if r.Verdict != VerdictLate && r.Verdict != VerdictApplied {
				t.Fatalf("%s: verdict %s (%s)", where, r.Verdict, r.Reason)
			}
			if r.Progress {
				t.Fatalf("%s: old seq reported progress", where)
			}
		}
		if d.event != nil {
			received[d.seq()] = true
		} else {
			snapCover = max(snapCover, d.seq())
		}
		maxSeen = max(maxSeen, d.seq())
		oracle.observe(d)

		s := m.State(at)
		if s.Seq != maxSeen {
			t.Fatalf("%s: head %d, want %d", where, s.Seq, maxSeen)
		}

		// 1. Merge rule: every field matches the last-writer-wins oracle.
		if oracle.statusSeq == 0 {
			if s.Status != StatusUnknown {
				t.Fatalf("%s: status %s before any was sent", where, s.Status)
			}
		} else if s.Status != oracle.status || s.StatusSeq != oracle.statusSeq {
			t.Fatalf("%s: status %s@%d, oracle %s@%d", where, s.Status, s.StatusSeq, oracle.status, oracle.statusSeq)
		}
		for _, o := range outcomes {
			os := outcomeOf(s, o)
			want, ok := oracle.prices[o]
			switch {
			case !ok && os.Price != nil:
				t.Fatalf("%s: %s has a price before any was sent", where, o)
			case ok && (os.Price == nil || *os.Price != want || os.Seq != oracle.seqs[o]):
				t.Fatalf("%s: %s = %v@%d, oracle %s@%d", where, o, os.Price, os.Seq, want, oracle.seqs[o])
			}
		}

		// 2. Safety: a consistent state equals the in-order truth at head.
		if s.Consistent {
			st, odds := tr.at(s.Seq)
			if s.Status != st {
				t.Fatalf("%s: consistent but status %s, truth %s at seq %d", where, s.Status, st, s.Seq)
			}
			for _, o := range outcomes {
				if p, _ := s.Price(o); p != odds[o] {
					t.Fatalf("%s: consistent but %s = %s, truth %s at seq %d", where, o, p, odds[o], s.Seq)
				}
			}
		}

		// 3. Precision: with nothing unaccounted for up to head, the market
		// must not raise a false alarm.
		accounted := true
		for seq := tr.base; seq <= s.Seq; seq++ {
			if !received[seq] && seq > snapCover {
				accounted = false
				break
			}
		}
		if accounted && !s.Consistent {
			t.Fatalf("%s: every seq up to %d is accounted for, yet inconsistent: gaps %v conflicts %v reasons %v",
				where, s.Seq, s.Gaps, s.Conflicts, s.Reasons)
		}

		// 4. Tradable only when proven current, open and complete.
		if s.Tradable {
			if !s.Consistent || s.Status != StatusOpen || s.Feed != FeedLive {
				t.Fatalf("%s: tradable state %+v", where, s)
			}
			for _, os := range s.Outcomes {
				if os.Price == nil {
					t.Fatalf("%s: tradable without a price for %s", where, os.ID)
				}
			}
		}
		if len(s.Conflicts) != 0 {
			t.Fatalf("%s: conflicts %v from a feed without contradictions", where, s.Conflicts)
		}
	}

	// 5. Liveness: a snapshot of the latest truth always restores consistency.
	m.ApplySnapshot(tr.snapshot(tr.last()), t0)
	if s := m.State(t0); !s.Consistent {
		t.Fatalf("not consistent after a final snapshot: %+v", s)
	}
}

func TestMergeInvariantsRandomFeeds(t *testing.T) {
	profiles := []struct {
		name string
		c    chaos
	}{
		{"in order", chaos{}},
		{"duplicates", chaos{dup: 0.3}},
		{"local reordering", chaos{dup: 0.1, shuffle: 3}},
		{"full shuffle", chaos{dup: 0.2, shuffle: -1}},
		{"loss", chaos{drop: 0.2}},
		{"loss and reordering", chaos{drop: 0.15, dup: 0.15, shuffle: 4}},
		{"heavy loss with snapshots", chaos{drop: 0.5, snap: 0.1, shuffle: 2}},
		{"everything", chaos{drop: 0.25, dup: 0.25, snap: 0.05, shuffle: -1}},
	}
	for pi, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(pi), 0x0dd5))
			for range 400 {
				runTrial(t, rng, 1+rng.IntN(40), p.c)
			}
		})
	}
}

// project drops everything that legitimately depends on arrival order or
// time (previous price, movement, timestamps) and keeps what must not.
func project(s State) string {
	out := fmt.Sprintf("seq=%d status=%s@%d consistent=%v tradable=%v gaps=%v conflicts=%v reasons=%v",
		s.Seq, s.Status, s.StatusSeq, s.Consistent, s.Tradable, s.Gaps, s.Conflicts, s.Reasons)
	for _, o := range s.Outcomes {
		out += fmt.Sprintf(" %s=%v@%d", o.ID, o.Price, o.Seq)
	}
	return out
}

func TestMergeIsOrderIndependent(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	for trial := range 500 {
		tr := genTruth(rng, 2+rng.IntN(30))
		deliveries := genDeliveries(rng, tr, chaos{drop: 0.2, dup: 0.2, snap: 0.05})
		var finals []string
		for range 4 {
			order := slices.Clone(deliveries)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			m := New(testDef, Config{StaleAfter: time.Hour})
			for _, d := range order {
				if d.event != nil {
					m.ApplyEvent(*d.event, t0)
				} else {
					m.ApplySnapshot(*d.snapshot, t0)
				}
			}
			finals = append(finals, project(m.State(t0)))
		}
		for _, f := range finals[1:] {
			if f != finals[0] {
				t.Fatalf("trial %d: order changed the result:\n%s\n%s", trial, finals[0], f)
			}
		}
	}
}

func TestSpecExampleAllPermutations(t *testing.T) {
	inOrder := newTestMarket()
	for _, raw := range specFeed {
		applyRaw(t, inOrder, raw, t0)
	}
	want := project(inOrder.State(t0))

	var permute func(prefix []int, rest []int)
	count := 0
	permute = func(prefix, rest []int) {
		if len(rest) == 0 {
			m := newTestMarket()
			for _, i := range prefix {
				applyRaw(t, m, specFeed[i], t0)
			}
			if got := project(m.State(t0)); got != want {
				t.Errorf("order %v: %s, want %s", prefix, got, want)
			}
			count++
			return
		}
		for i := range rest {
			next := slices.Concat(rest[:i:i], rest[i+1:])
			permute(append(slices.Clone(prefix), rest[i]), next)
		}
	}
	permute(nil, []int{0, 1, 2, 3, 4})
	if count != 120 {
		t.Fatalf("ran %d permutations", count)
	}
}

func FuzzMergeInvariants(f *testing.F) {
	f.Add(uint64(1), uint8(10), uint8(0), uint8(0), uint8(0), int8(0))
	f.Add(uint64(2), uint8(30), uint8(30), uint8(20), uint8(5), int8(-1))
	f.Add(uint64(3), uint8(40), uint8(60), uint8(10), uint8(10), int8(3))
	f.Fuzz(func(t *testing.T, seed uint64, n, drop, dup, snapPct uint8, shuffle int8) {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		c := chaos{
			drop:    float64(drop%90) / 100,
			dup:     float64(dup%90) / 100,
			snap:    float64(snapPct%50) / 100,
			shuffle: int(shuffle % 8),
		}
		runTrial(t, rng, 1+int(n%60), c)
	})
}
