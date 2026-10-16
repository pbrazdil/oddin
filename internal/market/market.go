package market

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// OutcomeDef describes one outcome of a market.
type OutcomeDef struct {
	ID   Outcome `json:"id"`
	Name string  `json:"name"`
}

// Definition is the static description of a market.
type Definition struct {
	Key      Key
	Event    string // e.g. "Falcons vs Wolves"
	Name     string // e.g. "Match winner"
	Outcomes []OutcomeDef
}

func (d Definition) hasOutcome(o Outcome) bool {
	return slices.ContainsFunc(d.Outcomes, func(od OutcomeDef) bool { return od.ID == o })
}

// Config tunes how the market judges feed health.
type Config struct {
	// StaleAfter is how long the feed may go without a new sequence number
	// before prices are considered stale.
	StaleAfter time.Duration
}

// Verdict is what the market did with an incoming message.
type Verdict string

const (
	// VerdictApplied: a new message that advanced the sequence head.
	VerdictApplied Verdict = "applied"
	// VerdictLate: a message older than the head that had not been seen
	// before. Merged field by field; values already superseded by a newer
	// message are kept.
	VerdictLate Verdict = "late"
	// VerdictDuplicate: an exact re-delivery of a seen message. Ignored.
	VerdictDuplicate Verdict = "duplicate"
	// VerdictConflict: a message that contradicts what was already
	// received for the same seq. Nothing from it is applied and the seq is
	// flagged so the market is not tradable until the state is re-proven.
	VerdictConflict Verdict = "conflict"
	// VerdictRejected: malformed or not for this market. Ignored.
	VerdictRejected Verdict = "rejected"
)

// Change records one field whose value changed.
type Change struct {
	Field string `json:"field"` // "status" or "odds.<outcome>"
	From  string `json:"from,omitzero"`
	To    string `json:"to"`
}

// Result describes the effect a message would have on the market. Results
// are computed without mutating the market (see PlanEvent) so the caller
// can durably journal the decision before committing it.
type Result struct {
	Verdict  Verdict
	Reason   string
	Seq      uint64
	Changes  []Change
	Progress bool // the message advanced the head, i.e. the feed is moving

	next *core  // state after commit; nil when nothing is committed
	seen string // fingerprint to remember for seq; "" for none
	base uint64 // market revision the plan was computed against
}

// Committable reports whether Commit would change the market.
func (r Result) Committable() bool { return r.next != nil }

type statusField struct {
	value     Status
	seq       uint64 // seq of the message that last stated it; 0 = never
	changedAt time.Time
}

type priceField struct {
	value     Price
	seq       uint64 // seq of the message that last stated it; 0 = never
	prev      Price  // value before the last change; 0 = none
	changedAt time.Time
}

// core is the part of the market state that a plan replaces wholesale.
// It is small enough to copy for every message.
type core struct {
	head         uint64    // highest seq accounted for
	lastProgress time.Time // when head last advanced
	status       statusField
	prices       map[Outcome]priceField
	// missing holds seqs <= head that have never been received and are not
	// covered by a snapshot. Any of them may have changed any field.
	missing Ranges
	// conflicts holds seqs for which contradicting content was received.
	conflicts Ranges
}

func (c core) clone() core {
	c.prices = maps.Clone(c.prices)
	return c // Ranges are immutable
}

// Market merges feed messages into the current state of one market.
//
// Merge rule: every field (the status and each outcome's price) is a
// last-writer-wins register ordered by seq. A message only overwrites a
// field the latest known value of which came from a lower seq. Arrival
// order therefore never matters: any permutation of the same messages
// yields the same state.
//
// Consistency rule: the state is only proven current when every field is
// known and no unaccounted seq (missing or conflicting) could have
// changed a field after the message that last set it. Concretely, every
// uncertain seq must be lower than the seq of every field. This is what
// stops a missed "suspended" from being hidden behind a later price-only
// update.
//
// Market is not safe for concurrent use.
type Market struct {
	def      Definition
	cfg      Config
	cur      core
	seen     map[uint64]string // seq -> fingerprint of every delta received
	revision uint64
}

// New returns an empty market that has not heard from the feed yet.
func New(def Definition, cfg Config) *Market {
	return &Market{
		def:  def,
		cfg:  cfg,
		cur:  core{prices: make(map[Outcome]priceField, len(def.Outcomes))},
		seen: make(map[uint64]string),
	}
}

// Definition returns the market's static description.
func (m *Market) Definition() Definition { return m.def }

// PlanEvent computes what applying ev at time at would do, without
// changing the market.
func (m *Market) PlanEvent(ev Event, at time.Time) Result {
	r := Result{Seq: ev.Seq, base: m.revision}
	if err := m.checkEvent(ev); err != nil {
		r.Verdict, r.Reason = VerdictRejected, err.Error()
		return r
	}

	fp := fingerprint(ev.Status, ev.Odds)
	if prev, ok := m.seen[ev.Seq]; ok {
		if prev == fp {
			r.Verdict, r.Reason = VerdictDuplicate, fmt.Sprintf("seq %d was already received with identical content", ev.Seq)
			return r
		}
		return m.conflict(r, "", fmt.Sprintf("seq %d was already received with different content", ev.Seq))
	}

	next := m.cur.clone()
	changes, contested := next.merge(m.def, ev.Seq, ev.Status, ev.Odds, at)
	if contested != "" {
		return m.conflict(r, fp, fmt.Sprintf("%s at seq %d contradicts the snapshot taken at that seq", contested, ev.Seq))
	}

	switch {
	case ev.Seq > next.head:
		if ev.Seq > next.head+1 {
			next.missing = next.missing.Add(Range{next.head + 1, ev.Seq - 1})
		}
		next.head = ev.Seq
		next.lastProgress = at
		r.Verdict, r.Progress = VerdictApplied, true
	default:
		next.missing = next.missing.Remove(Range{ev.Seq, ev.Seq})
		r.Verdict = VerdictLate
		r.Reason = fmt.Sprintf("arrived after seq %d", next.head)
	}
	r.Changes, r.next, r.seen = changes, &next, fp
	return r
}

// PlanSnapshot computes what applying a full snapshot would do. A snapshot
// vouches for the whole history up to its seq, so it clears every gap and
// conflict at or below that seq.
func (m *Market) PlanSnapshot(s Snapshot, at time.Time) Result {
	r := Result{Seq: s.Seq, base: m.revision}
	if err := m.checkSnapshot(s); err != nil {
		r.Verdict, r.Reason = VerdictRejected, err.Error()
		return r
	}

	next := m.cur.clone()
	changes, contested := next.merge(m.def, s.Seq, s.Status, s.Odds, at)
	if contested != "" {
		return m.conflict(r, "", fmt.Sprintf("snapshot %s at seq %d contradicts the event received for that seq", contested, s.Seq))
	}
	next.missing = next.missing.Remove(Range{1, s.Seq})
	next.conflicts = next.conflicts.Remove(Range{1, s.Seq})
	if s.Seq > next.head {
		next.head = s.Seq
		next.lastProgress = at
		r.Progress = true
	}
	r.Verdict, r.Reason = VerdictApplied, fmt.Sprintf("resynced history through seq %d", s.Seq)
	r.Changes, r.next = changes, &next
	return r
}

func (m *Market) conflict(r Result, fp, reason string) Result {
	next := m.cur.clone()
	next.conflicts = next.conflicts.Add(Range{r.Seq, r.Seq})
	r.Verdict, r.Reason, r.next, r.seen = VerdictConflict, reason, &next, fp
	return r
}

// Commit applies a plan returned by PlanEvent or PlanSnapshot. It panics if
// the market changed since the plan was computed. Plans that change
// nothing (rejections, duplicates) are no-ops.
func (m *Market) Commit(r Result) {
	if r.next == nil {
		return
	}
	if r.base != m.revision {
		panic("market: commit of a stale plan")
	}
	m.cur = *r.next
	if r.seen != "" {
		m.seen[r.Seq] = r.seen
	}
	m.revision++
}

// ApplyEvent plans and commits ev in one step.
func (m *Market) ApplyEvent(ev Event, at time.Time) Result {
	r := m.PlanEvent(ev, at)
	m.Commit(r)
	return r
}

// ApplySnapshot plans and commits s in one step.
func (m *Market) ApplySnapshot(s Snapshot, at time.Time) Result {
	r := m.PlanSnapshot(s, at)
	m.Commit(r)
	return r
}

func (m *Market) checkEvent(ev Event) error {
	if ev.Key != m.def.Key {
		return fmt.Errorf("event is for market %s, not %s", ev.Key, m.def.Key)
	}
	if ev.Seq == 0 || ev.Seq > MaxSeq {
		return fmt.Errorf("seq %d out of range", ev.Seq)
	}
	if ev.Status != "" {
		if _, err := ParseStatus(string(ev.Status)); err != nil {
			return err
		}
	}
	for o, p := range ev.Odds {
		if !m.def.hasOutcome(o) {
			return fmt.Errorf("unknown outcome %q", o)
		}
		if p < MinPrice || p > MaxPrice {
			return fmt.Errorf("odds.%s %s out of range", o, p)
		}
	}
	return nil
}

func (m *Market) checkSnapshot(s Snapshot) error {
	if err := m.checkEvent(Event{Key: s.Key, Seq: s.Seq, Status: s.Status, Odds: s.Odds}); err != nil {
		return err
	}
	if s.Status == "" {
		return fmt.Errorf("snapshot has no status")
	}
	for _, od := range m.def.Outcomes {
		if _, ok := s.Odds[od.ID]; !ok {
			return fmt.Errorf("snapshot has no odds for %q", od.ID)
		}
	}
	return nil
}

// merge applies the last-writer-wins rule for one message. It is atomic:
// if any field contradicts a value stated with the same seq, nothing is
// changed and the contested field is returned.
func (c *core) merge(def Definition, seq uint64, st Status, odds map[Outcome]Price, at time.Time) ([]Change, string) {
	if st != "" && c.status.seq == seq && c.status.value != st {
		return nil, "status"
	}
	for _, od := range def.Outcomes {
		if p, ok := odds[od.ID]; ok {
			if f := c.prices[od.ID]; f.seq == seq && f.value != p {
				return nil, "odds." + string(od.ID)
			}
		}
	}

	var changes []Change
	if st != "" && seq > c.status.seq {
		if st != c.status.value {
			ch := Change{Field: "status", To: string(st)}
			if c.status.seq > 0 {
				ch.From = string(c.status.value)
			}
			changes = append(changes, ch)
			c.status.changedAt = at
		}
		c.status.value, c.status.seq = st, seq
	}
	for _, od := range def.Outcomes {
		p, ok := odds[od.ID]
		if !ok {
			continue
		}
		f := c.prices[od.ID]
		if seq <= f.seq {
			continue
		}
		if p != f.value || f.seq == 0 {
			ch := Change{Field: "odds." + string(od.ID), To: p.String()}
			if f.seq > 0 {
				ch.From = f.value.String()
				f.prev = f.value
			}
			changes = append(changes, ch)
			f.changedAt = at
		}
		f.value, f.seq = p, seq
		c.prices[od.ID] = f
	}
	return changes, ""
}

// fingerprint is a canonical encoding of a delta's content, used to tell an
// exact re-delivery from a conflicting one.
func fingerprint(st Status, odds map[Outcome]Price) string {
	var b strings.Builder
	b.WriteString("status=")
	b.WriteString(string(st))
	for _, o := range slices.Sorted(maps.Keys(odds)) {
		fmt.Fprintf(&b, ";%s=%d", o, odds[o])
	}
	return b.String()
}
