package market

import "time"

// FeedHealth says whether the feed is still delivering new messages.
type FeedHealth string

const (
	FeedWaiting FeedHealth = "waiting" // nothing received yet
	FeedLive    FeedHealth = "live"
	FeedStale   FeedHealth = "stale" // no new seq within Config.StaleAfter
)

// Reason explains why a market is not tradable.
type Reason string

const (
	ReasonNoData      Reason = "no_data"      // nothing received yet
	ReasonIncomplete  Reason = "incomplete"   // status or a price has never been received
	ReasonSequenceGap Reason = "sequence_gap" // a missing seq may have changed a field
	ReasonConflict    Reason = "conflict"     // contradicting content for a relevant seq
	ReasonFeedStale   Reason = "feed_stale"
	ReasonSuspended   Reason = "suspended"
	ReasonClosed      Reason = "closed"
)

// Move is the direction of an outcome's last price change.
type Move string

const (
	MoveUp   Move = "up"
	MoveDown Move = "down"
)

// OutcomeState is the public view of one outcome.
type OutcomeState struct {
	ID        Outcome   `json:"id"`
	Name      string    `json:"name"`
	Price     *Price    `json:"price"` // null until the feed has sent one
	Seq       uint64    `json:"seq,omitzero"`
	Prev      *Price    `json:"prev,omitzero"`
	Move      Move      `json:"move,omitzero"`
	ChangedAt time.Time `json:"changedAt,omitzero"`
}

// State is the public view of a market at a point in time. It is the
// single source of truth that clients render; they never merge deltas.
type State struct {
	MatchID     string         `json:"matchId"`
	MarketID    string         `json:"marketId"`
	Event       string         `json:"event"`
	Market      string         `json:"market"`
	Seq         uint64         `json:"seq"` // highest seq accounted for
	Status      Status         `json:"status"`
	StatusSeq   uint64         `json:"statusSeq,omitzero"`
	Outcomes    []OutcomeState `json:"outcomes"`
	Feed        FeedHealth     `json:"feed"`
	LastEventAt time.Time      `json:"lastEventAt,omitzero"`
	// Consistent is true when the state provably equals what applying every
	// feed message up to Seq in order would give.
	Consistent bool   `json:"consistent"`
	Gaps       Ranges `json:"gaps"`      // missing seqs that may affect the state
	Conflicts  Ranges `json:"conflicts"` // contradicting seqs that may affect the state
	// Tradable is the only flag a client may use to make prices actionable.
	Tradable bool     `json:"tradable"`
	Reasons  []Reason `json:"reasons"`
}

// Price returns the price of outcome o, if known.
func (s State) Price(o Outcome) (Price, bool) {
	for _, os := range s.Outcomes {
		if os.ID == o && os.Price != nil {
			return *os.Price, true
		}
	}
	return 0, false
}

// State evaluates the market at time at.
func (m *Market) State(at time.Time) State { return m.stateOf(&m.cur, at) }

// Preview evaluates the state the market would have after committing r.
func (m *Market) Preview(r Result, at time.Time) State {
	if r.next == nil {
		return m.State(at)
	}
	return m.stateOf(r.next, at)
}

func (m *Market) stateOf(c *core, at time.Time) State {
	s := State{
		MatchID:  m.def.Key.MatchID,
		MarketID: m.def.Key.MarketID,
		Event:    m.def.Event,
		Market:   m.def.Name,
		Seq:      c.head,
		Status:   StatusUnknown,
		Outcomes: make([]OutcomeState, 0, len(m.def.Outcomes)),
		Gaps:     Ranges{},
		Reasons:  []Reason{},
	}
	if c.status.seq > 0 {
		s.Status, s.StatusSeq = c.status.value, c.status.seq
	}

	// The lowest seq among all fields bounds which uncertain seqs matter:
	// an uncertain seq below it cannot be the latest writer of any field.
	complete := c.status.seq > 0
	floor := c.status.seq
	for _, od := range m.def.Outcomes {
		os := OutcomeState{ID: od.ID, Name: od.Name}
		f := c.prices[od.ID]
		if f.seq == 0 {
			complete = false
		} else {
			p := f.value
			os.Price, os.Seq, os.ChangedAt = &p, f.seq, f.changedAt
			if f.prev != 0 {
				prev := f.prev
				os.Prev = &prev
				switch {
				case f.value > f.prev:
					os.Move = MoveUp
				case f.value < f.prev:
					os.Move = MoveDown
				}
			}
			floor = min(floor, f.seq)
		}
		s.Outcomes = append(s.Outcomes, os)
	}
	if !complete {
		floor = 0
	}
	s.Gaps = c.missing.AtLeast(floor)
	s.Conflicts = c.conflicts.AtLeast(floor)
	s.Consistent = complete && len(s.Gaps) == 0 && len(s.Conflicts) == 0

	switch {
	case c.head == 0:
		s.Feed = FeedWaiting
	case at.Sub(c.lastProgress) > m.cfg.StaleAfter:
		s.Feed = FeedStale
	default:
		s.Feed = FeedLive
	}
	if c.head > 0 {
		s.LastEventAt = c.lastProgress
	}

	if c.head == 0 {
		s.Reasons = append(s.Reasons, ReasonNoData)
	} else {
		if !complete {
			s.Reasons = append(s.Reasons, ReasonIncomplete)
		}
		if len(s.Gaps) > 0 {
			s.Reasons = append(s.Reasons, ReasonSequenceGap)
		}
		if len(s.Conflicts) > 0 {
			s.Reasons = append(s.Reasons, ReasonConflict)
		}
		if s.Feed == FeedStale {
			s.Reasons = append(s.Reasons, ReasonFeedStale)
		}
		switch s.Status {
		case StatusSuspended:
			s.Reasons = append(s.Reasons, ReasonSuspended)
		case StatusClosed:
			s.Reasons = append(s.Reasons, ReasonClosed)
		}
	}
	s.Tradable = len(s.Reasons) == 0 && s.Consistent && s.Status == StatusOpen && s.Feed == FeedLive
	return s
}
