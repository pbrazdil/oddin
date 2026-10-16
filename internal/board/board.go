// Package board owns the live markets: it routes feed messages to them,
// journals every decision before it takes effect, versions and publishes
// the resulting states, watches feed health and resyncs from snapshots.
package board

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"oddsboard/internal/journal"
	"oddsboard/internal/market"
)

// Source says where a message came from. It is recorded in the journal.
type Source string

const (
	SourceFeed      Source = "feed"
	SourceAdmin     Source = "admin"
	SourceReplay    Source = "replay"
	SourceSimulator Source = "simulator"
	SourceRecovery  Source = "recovery"
	SourceSystem    Source = "system"
)

// ErrUnavailable means the journal could not record a message, so the
// message was not applied. The sender should retry; delivery is idempotent.
var ErrUnavailable = errors.New("board unavailable")

// MaxBatch caps how many messages one submission may carry.
const MaxBatch = 1000

// SnapshotSource fetches the provider's current full state of a market.
// It returns the raw message so it can be journaled exactly as received.
type SnapshotSource interface {
	Snapshot(ctx context.Context, key market.Key) ([]byte, error)
}

// Config tunes the board.
type Config struct {
	StaleAfter   time.Duration // feed silence before prices count as stale
	RecoverAfter time.Duration // how long a market may be inconsistent before a snapshot is requested
	RecoverEvery time.Duration // minimum interval between snapshot requests per market
	Now          func() time.Time
	Log          *slog.Logger
}

func (c *Config) defaults() {
	if c.StaleAfter == 0 {
		c.StaleAfter = 3 * time.Second
	}
	if c.RecoverAfter == 0 {
		c.RecoverAfter = 2 * time.Second
	}
	if c.RecoverEvery == 0 {
		c.RecoverEvery = 5 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Published is a state as delivered to clients. Within one epoch, a higher
// version is always a newer state; a new epoch (after a reset) starts a new
// history that clients must adopt wholesale.
type Published struct {
	Epoch        string       `json:"epoch"`
	Version      uint64       `json:"version"`
	StaleAfterMs int64        `json:"staleAfterMs"`
	State        market.State `json:"state"`
	JSON         []byte       `json:"-"`
}

// Ack reports what happened to one submitted message.
type Ack struct {
	Seq      uint64         `json:"seq,omitzero"`
	Verdict  market.Verdict `json:"verdict"`
	Reason   string         `json:"reason,omitzero"`
	Version  uint64         `json:"version,omitzero"`
	RecordID uint64         `json:"recordId,omitzero"`
}

type slot struct {
	def       market.Definition
	m         *market.Market
	epoch     string
	version   uint64
	stateJSON []byte // last published state, for change detection
	current   atomic.Pointer[Published]
	subs      map[chan struct{}]struct{}

	badSince     time.Time // when the market last became inconsistent
	lastRecovery time.Time
	recovering   bool
}

// Board is safe for concurrent use.
type Board struct {
	cfg       Config
	journal   *journal.Journal
	snapshots SnapshotSource

	mu    sync.Mutex
	slots map[market.Key]*slot
	keys  []market.Key

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a board for the given markets, rebuilding their state from
// the journal. Every journaled decision is re-derived and compared with what
// was recorded; any divergence is an error, so the board never starts from
// a state it cannot reproduce.
func New(defs []market.Definition, cfg Config, j *journal.Journal, snapshots SnapshotSource) (*Board, error) {
	cfg.defaults()
	b := newBoard(defs, cfg, j, snapshots)
	if err := b.restore(j.Records()); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range b.keys {
		if s := b.slots[k]; s.epoch == "" {
			if err := b.resetLocked(s, SourceSystem, "Market initialised"); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func newBoard(defs []market.Definition, cfg Config, j *journal.Journal, snapshots SnapshotSource) *Board {
	b := &Board{cfg: cfg, journal: j, snapshots: snapshots, slots: map[market.Key]*slot{}}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	for _, d := range defs {
		b.slots[d.Key] = &slot{def: d, m: market.New(d, b.marketConfig()), subs: map[chan struct{}]struct{}{}}
		b.keys = append(b.keys, d.Key)
	}
	return b
}

func (b *Board) marketConfig() market.Config { return market.Config{StaleAfter: b.cfg.StaleAfter} }

// now returns wall-clock UTC without a monotonic reading, so that states
// computed live and states recomputed from the journal are byte-identical.
func (b *Board) now() time.Time { return b.cfg.Now().UTC() }

// Close stops background work.
func (b *Board) Close() {
	b.cancel()
	b.wg.Wait()
}

// Run drives feed-health checks and snapshot recovery until ctx ends.
func (b *Board) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Tick()
		}
	}
}

// Markets returns the markets in configuration order.
func (b *Board) Markets() []market.Definition {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]market.Definition, 0, len(b.keys))
	for _, k := range b.keys {
		out = append(out, b.slots[k].def)
	}
	return out
}

// Current returns the latest published state of a market.
func (b *Board) Current(key market.Key) (*Published, bool) {
	b.mu.Lock()
	s, ok := b.slots[key]
	b.mu.Unlock()
	if !ok {
		return nil, false
	}
	return s.current.Load(), true
}

// Subscribe returns a channel that receives a signal whenever the market
// publishes a new state. Signals coalesce: a slow subscriber skips
// intermediate states but always catches up with the latest via Current.
func (b *Board) Subscribe(key market.Key) (<-chan struct{}, func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.slots[key]
	if !ok {
		return nil, nil, false
	}
	ch := make(chan struct{}, 1)
	s.subs[ch] = struct{}{}
	return ch, func() {
		b.mu.Lock()
		delete(s.subs, ch)
		b.mu.Unlock()
	}, true
}

// SubmitBatch processes a JSON object or array of feed events in order.
func (b *Board) SubmitBatch(body []byte, src Source) ([]Ack, error) {
	items, err := market.SplitBatch(body)
	if err == nil && len(items) > MaxBatch {
		err = fmt.Errorf("batch of %d messages exceeds the limit of %d", len(items), MaxBatch)
	}
	if err != nil {
		b.mu.Lock()
		_, jerr := b.recordUnroutableLocked(body, src, err.Error())
		b.mu.Unlock()
		if jerr != nil {
			return nil, jerr
		}
		return nil, err
	}
	acks := make([]Ack, 0, len(items))
	for _, item := range items {
		ack, err := b.SubmitEvent(item, src)
		if err != nil {
			return acks, err
		}
		acks = append(acks, ack)
	}
	return acks, nil
}

// SubmitEvent processes one feed event.
func (b *Board) SubmitEvent(raw []byte, src Source) (Ack, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.submitLocked(journal.KindEvent, raw, src)
}

// SubmitSnapshot processes one full-state snapshot.
func (b *Board) SubmitSnapshot(raw []byte, src Source) (Ack, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.submitLocked(journal.KindSnapshot, raw, src)
}

func (b *Board) submitLocked(kind journal.Kind, raw []byte, src Source) (Ack, error) {
	now := b.now()
	key, seq := market.Peek(raw)
	s, ok := b.slots[key]
	if !ok {
		reason := "unknown market " + key.String()
		if key == (market.Key{}) {
			reason = "message does not name a market"
		}
		if err := parseErr(kind, raw); err != nil {
			reason = err.Error()
		}
		return b.recordUnroutableLocked(raw, src, reason)
	}
	res := plan(s.m, kind, raw, now)
	if res.Seq == 0 {
		res.Seq = seq
	}
	return b.commitLocked(s, kind, res, raw, src, now)
}

func parseErr(kind journal.Kind, raw []byte) error {
	var err error
	if kind == journal.KindSnapshot {
		_, err = market.ParseSnapshot(raw)
	} else {
		_, err = market.ParseEvent(raw)
	}
	return err
}

// plan parses raw and computes its effect. It is shared by the live path
// and journal replay so that both decide identically.
func plan(m *market.Market, kind journal.Kind, raw []byte, at time.Time) market.Result {
	switch kind {
	case journal.KindSnapshot:
		snap, err := market.ParseSnapshot(raw)
		if err != nil {
			return market.Result{Verdict: market.VerdictRejected, Reason: err.Error()}
		}
		return m.PlanSnapshot(snap, at)
	default:
		ev, err := market.ParseEvent(raw)
		if err != nil {
			return market.Result{Verdict: market.VerdictRejected, Reason: err.Error()}
		}
		return m.PlanEvent(ev, at)
	}
}

// commitLocked journals a decision and only then applies and publishes it.
// If the journal write fails, nothing changes.
func (b *Board) commitLocked(s *slot, kind journal.Kind, res market.Result, raw []byte, src Source, now time.Time) (Ack, error) {
	before := s.m.State(now)
	after := s.m.Preview(res, now)
	version, stJSON, changed := s.next(after)
	rec, err := b.journal.Append(journal.Record{
		At:       now,
		Kind:     kind,
		Source:   string(src),
		MatchID:  s.def.Key.MatchID,
		MarketID: s.def.Key.MarketID,
		Seq:      res.Seq,
		Payload:  string(raw),
		Verdict:  res.Verdict,
		Reason:   res.Reason,
		Changes:  res.Changes,
		Summary:  summarize(s.def, kind, res, before, after),
		Epoch:    s.epoch,
		Version:  version,
		State:    &after,
	})
	if err != nil {
		b.cfg.Log.Error("journal append failed; message not applied", "market", s.def.Key, "seq", res.Seq, "err", err)
		return Ack{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.m.Commit(res)
	if changed {
		b.publishLocked(s, version, after, stJSON)
	}
	b.trackConsistencyLocked(s, after, now)
	return Ack{Seq: res.Seq, Verdict: res.Verdict, Reason: res.Reason, Version: s.version, RecordID: rec.ID}, nil
}

// recordUnroutableLocked journals a message that belongs to no known market.
func (b *Board) recordUnroutableLocked(raw []byte, src Source, reason string) (Ack, error) {
	key, seq := market.Peek(raw)
	rec, err := b.journal.Append(journal.Record{
		At:       b.now(),
		Kind:     journal.KindEvent,
		Source:   string(src),
		MatchID:  key.MatchID,
		MarketID: key.MarketID,
		Seq:      seq,
		Payload:  string(raw),
		Verdict:  market.VerdictRejected,
		Reason:   reason,
		Summary:  "Rejected: " + reason,
	})
	if err != nil {
		return Ack{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return Ack{Seq: seq, Verdict: market.VerdictRejected, Reason: reason, RecordID: rec.ID}, nil
}

// next reports the version a state would be published under.
func (s *slot) next(st market.State) (uint64, []byte, bool) {
	stJSON, err := json.Marshal(st)
	if err != nil {
		panic(err) // State contains only marshalable types
	}
	if bytes.Equal(stJSON, s.stateJSON) {
		return s.version, stJSON, false
	}
	return s.version + 1, stJSON, true
}

func (b *Board) publishLocked(s *slot, version uint64, st market.State, stJSON []byte) {
	s.version, s.stateJSON = version, stJSON
	p := &Published{Epoch: s.epoch, Version: version, StaleAfterMs: b.cfg.StaleAfter.Milliseconds(), State: st}
	var err error
	if p.JSON, err = json.Marshal(p); err != nil {
		panic(err)
	}
	s.current.Store(p)
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Tick re-evaluates every market against the clock: it publishes (and
// journals) feed-health transitions and triggers snapshot recovery.
func (b *Board) Tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for _, k := range b.keys {
		s := b.slots[k]
		before := market.State{}
		if p := s.current.Load(); p != nil {
			before = p.State
		}
		st := s.m.State(now)
		if version, stJSON, changed := s.next(st); changed {
			_, err := b.journal.Append(journal.Record{
				At:       now,
				Kind:     journal.KindHealth,
				Source:   string(SourceSystem),
				MatchID:  k.MatchID,
				MarketID: k.MarketID,
				Seq:      st.Seq,
				Summary:  healthSummary(before, st, b.cfg.StaleAfter),
				Epoch:    s.epoch,
				Version:  version,
				State:    &st,
			})
			if err != nil {
				// Withholding a less tradable state would be worse than an
				// unrecorded one: publish anyway, fail closed.
				b.cfg.Log.Error("journal append failed for health change; publishing anyway", "market", k, "err", err)
			}
			b.publishLocked(s, version, st, stJSON)
		}
		b.trackConsistencyLocked(s, st, now)
		b.maybeRecoverLocked(s, now)
	}
}

func (b *Board) trackConsistencyLocked(s *slot, st market.State, now time.Time) {
	if st.Consistent {
		s.badSince = time.Time{}
	} else if s.badSince.IsZero() {
		s.badSince = now
	}
}

func (b *Board) maybeRecoverLocked(s *slot, now time.Time) {
	switch {
	case b.snapshots == nil, s.recovering, s.badSince.IsZero():
		return
	case now.Sub(s.badSince) < b.cfg.RecoverAfter:
		return
	case !s.lastRecovery.IsZero() && now.Sub(s.lastRecovery) < b.cfg.RecoverEvery:
		return
	}
	s.recovering, s.lastRecovery = true, now
	key := s.def.Key
	b.wg.Go(func() { b.resync(key) })
}

func (b *Board) resync(key market.Key) {
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	raw, err := b.snapshots.Snapshot(ctx, key)

	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.slots[key]
	s.recovering = false
	if b.ctx.Err() != nil {
		return
	}
	if err != nil {
		b.cfg.Log.Warn("snapshot request failed", "market", key, "err", err)
		if _, jerr := b.journal.Append(journal.Record{
			At:       b.now(),
			Kind:     journal.KindRecovery,
			Source:   string(SourceRecovery),
			MatchID:  key.MatchID,
			MarketID: key.MarketID,
			Reason:   err.Error(),
			Summary:  "Snapshot request failed: " + err.Error(),
			Epoch:    s.epoch,
			Version:  s.version,
		}); jerr != nil {
			b.cfg.Log.Error("journal append failed", "err", jerr)
		}
		return
	}
	if _, err := b.submitLocked(journal.KindSnapshot, raw, SourceRecovery); err != nil {
		b.cfg.Log.Error("applying recovery snapshot failed", "market", key, "err", err)
	}
}

// Reset discards a market's state and starts a new epoch. Intended for
// test and replay sessions; the old history stays in the journal.
func (b *Board) Reset(key market.Key, src Source) (*Published, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.slots[key]
	if !ok {
		return nil, fmt.Errorf("unknown market %s", key)
	}
	if err := b.resetLocked(s, src, fmt.Sprintf("Market reset (%s)", src)); err != nil {
		return nil, err
	}
	return s.current.Load(), nil
}

func (b *Board) resetLocked(s *slot, src Source, summary string) error {
	now := b.now()
	m := market.New(s.def, b.marketConfig())
	st := m.State(now)
	stJSON, _ := json.Marshal(st)
	epoch := rand.Text()[:12]
	version := s.version + 1
	if _, err := b.journal.Append(journal.Record{
		At:       now,
		Kind:     journal.KindReset,
		Source:   string(src),
		MatchID:  s.def.Key.MatchID,
		MarketID: s.def.Key.MarketID,
		Summary:  summary,
		Epoch:    epoch,
		Version:  version,
		State:    &st,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.m, s.epoch, s.badSince, s.lastRecovery = m, epoch, time.Time{}, time.Time{}
	b.publishLocked(s, version, st, stJSON)
	return nil
}

// restore replays journal records, checking each recorded verdict and state.
func (b *Board) restore(records []journal.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range records {
		s, ok := b.slots[r.Key()]
		if !ok {
			continue // e.g. a rejected message for an unknown market
		}
		switch r.Kind {
		case journal.KindReset:
			s.m = market.New(s.def, b.marketConfig())
		case journal.KindEvent, journal.KindSnapshot:
			res := plan(s.m, r.Kind, []byte(r.Payload), r.At)
			if res.Verdict != r.Verdict {
				return fmt.Errorf("journal record %d: replay gives verdict %q, journal says %q (%s)", r.ID, res.Verdict, r.Verdict, res.Reason)
			}
			s.m.Commit(res)
		}
		s.epoch = r.Epoch
		if r.State == nil {
			continue
		}
		got, _ := json.Marshal(s.m.State(r.At))
		want, _ := json.Marshal(*r.State)
		if !bytes.Equal(got, want) {
			return fmt.Errorf("journal record %d: replayed state differs from the recorded one\nreplayed: %s\nrecorded: %s", r.ID, got, want)
		}
		s.version = r.Version
		b.publishLocked(s, r.Version, *r.State, want)
	}
	return nil
}

// VerifyReport is the outcome of Verify.
type VerifyReport struct {
	Records     int    `json:"records"`
	Head        string `json:"head"`
	ChainOK     bool   `json:"chainOk"`
	ChainError  string `json:"chainError,omitzero"`
	ReplayOK    bool   `json:"replayOk"`
	ReplayError string `json:"replayError,omitzero"`
}

// Verify checks the journal's hash chain and replays it from scratch,
// confirming that every recorded verdict and state is reproducible.
func (b *Board) Verify() VerifyReport {
	var rep VerifyReport
	n, err := b.journal.Verify()
	rep.Records, rep.Head, rep.ChainOK = n, b.journal.Head(), err == nil
	if err != nil {
		rep.ChainError = err.Error()
	}
	var defs []market.Definition
	for _, d := range b.Markets() {
		defs = append(defs, d)
	}
	fresh := newBoard(defs, b.cfg, nil, nil)
	if err := fresh.restore(b.journal.Records()); err != nil {
		rep.ReplayError = err.Error()
	} else {
		rep.ReplayOK = true
	}
	return rep
}

// History returns journal records for one market with ID > after.
func (b *Board) History(key market.Key, after uint64, limit int) []journal.Record {
	return b.journal.Query(after, limit, func(r journal.Record) bool { return r.Key() == key })
}

// StateAt returns the record that determined what the board showed for a
// market at time t.
func (b *Board) StateAt(key market.Key, t time.Time) (journal.Record, bool) {
	return b.journal.LastBefore(t, func(r journal.Record) bool { return r.Key() == key && r.State != nil })
}

// JournalHead returns the hash committing to the whole journal.
func (b *Board) JournalHead() string { return b.journal.Head() }
