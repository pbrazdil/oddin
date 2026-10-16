package board

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"oddsboard/internal/journal"
	"oddsboard/internal/market"
)

var (
	key = market.Key{MatchID: "match-1", MarketID: "winner"}
	def = market.Definition{
		Key: key, Event: "Falcons vs Wolves", Name: "Match winner",
		Outcomes: []market.OutcomeDef{{ID: "falcons", Name: "Falcons"}, {ID: "wolves", Name: "Wolves"}},
	}
	t0      = time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
)

var specFeed = []string{
	`{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`,
	`{"matchId":"match-1","marketId":"winner","seq":42,"odds":{"falcons":1.80}}`,
	`{"matchId":"match-1","marketId":"winner","seq":43,"status":"suspended"}`,
	`{"matchId":"match-1","marketId":"winner","seq":44,"status":"open"}`,
	`{"matchId":"match-1","marketId":"winner","seq":45,"odds":{"falcons":1.90,"wolves":1.95}}`,
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	b   *Board
	j   *journal.Journal
	clk *clock
}

func newFixture(t *testing.T, j *journal.Journal, src SnapshotSource) fixture {
	t.Helper()
	clk := &clock{t: t0}
	if j == nil {
		j = journal.NewMemory()
	}
	b, err := New([]market.Definition{def}, Config{
		StaleAfter: 3 * time.Second, RecoverAfter: 2 * time.Second, RecoverEvery: 5 * time.Second,
		Now: clk.Now, Log: discard,
	}, j, src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return fixture{b, j, clk}
}

func (f fixture) submit(t *testing.T, raw string) Ack {
	t.Helper()
	ack, err := f.b.SubmitEvent([]byte(raw), SourceFeed)
	if err != nil {
		t.Fatalf("SubmitEvent(%s): %v", raw, err)
	}
	return ack
}

func (f fixture) current(t *testing.T) *Published {
	t.Helper()
	p, ok := f.b.Current(key)
	if !ok || p == nil {
		t.Fatal("no published state")
	}
	return p
}

func price(p *Published, o market.Outcome) string {
	v, ok := p.State.Price(o)
	if !ok {
		return "-"
	}
	return v.String()
}

func TestNewBoardStartsEmptyAndJournalsInit(t *testing.T) {
	f := newFixture(t, nil, nil)
	p := f.current(t)
	if p.Version != 1 || p.Epoch == "" || p.State.Tradable || p.State.Feed != market.FeedWaiting {
		t.Fatalf("initial publish: %+v", p)
	}
	recs := f.j.Records()
	if len(recs) != 1 || recs[0].Kind != journal.KindReset || recs[0].Summary != "Market initialised" {
		t.Fatalf("journal: %+v", recs)
	}
	var wire map[string]any
	if err := json.Unmarshal(p.JSON, &wire); err != nil || wire["epoch"] != p.Epoch || wire["staleAfterMs"] != float64(3000) {
		t.Fatalf("wire JSON %s: %v", p.JSON, err)
	}
}

func TestSubmitVersionsAndPublishes(t *testing.T) {
	f := newFixture(t, nil, nil)
	for i, raw := range specFeed {
		ack := f.submit(t, raw)
		if ack.Verdict != market.VerdictApplied || ack.Version != uint64(i+2) || ack.RecordID != uint64(i+2) {
			t.Fatalf("seq %d ack %+v", 41+i, ack)
		}
	}
	p := f.current(t)
	if p.Version != 6 || price(p, "falcons") != "1.90" || price(p, "wolves") != "1.95" || !p.State.Tradable {
		t.Fatalf("final %+v", p.State)
	}

	// Re-delivery is journaled but neither changes nor re-publishes the state.
	ack := f.submit(t, specFeed[2])
	if ack.Verdict != market.VerdictDuplicate || ack.Version != 6 {
		t.Fatalf("duplicate ack %+v", ack)
	}
	if f.current(t) != p {
		t.Fatal("duplicate re-published")
	}
	if n := f.j.Len(); n != 7 {
		t.Fatalf("journal has %d records, want 7", n)
	}
}

func TestSummariesExplainEachStep(t *testing.T) {
	f := newFixture(t, nil, nil)
	for _, raw := range specFeed {
		f.submit(t, raw)
	}
	f.submit(t, `{"matchId":"match-1","marketId":"winner","seq":48,"odds":{"wolves":2.00}}`)
	f.submit(t, `{"matchId":"match-1","marketId":"winner","seq":48,"odds":{"wolves":2.10}}`)
	f.submit(t, `{"matchId":"match-1","marketId":"winner","seq":49,"odds":{"draw":3.00}}`)
	want := []string{
		"Market initialised",
		"Market open; Falcons 1.85; Wolves 2.05; now tradable",
		"Falcons 1.85 → 1.80",
		"Market suspended; trading locked: market suspended",
		"Market reopened; now tradable",
		"Falcons 1.80 → 1.90; Wolves 2.05 → 1.95",
		"Wolves 1.95 → 2.00; missing seq 46–47; trading locked: missing feed messages",
		"Conflict: seq 48 was already received with different content; trading locked until the state is re-proven",
		`Rejected: unknown outcome "draw"`,
	}
	recs := f.j.Records()
	for i, w := range want {
		if recs[i].Summary != w {
			t.Errorf("record %d summary:\n got %q\nwant %q", i+1, recs[i].Summary, w)
		}
	}
}

func TestJournalFailureMeansNothingApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, err := journal.Open(path, discard)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, j, nil)
	f.submit(t, specFeed[0])
	before := f.current(t)

	j.Close() // every further append fails
	_, err = f.b.SubmitEvent([]byte(specFeed[1]), SourceFeed)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if after := f.current(t); after != before || price(after, "falcons") != "1.85" {
		t.Fatalf("state changed without a journal record: %+v", after.State)
	}
}

func TestFeedStalenessIsPublishedAndJournaled(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.submit(t, specFeed[0])
	f.clk.Advance(3 * time.Second)
	f.b.Tick()
	if p := f.current(t); p.State.Feed != market.FeedLive || p.Version != 2 {
		t.Fatalf("live at exactly StaleAfter expected: %+v", p)
	}

	f.clk.Advance(time.Second)
	f.b.Tick()
	p := f.current(t)
	if p.State.Feed != market.FeedStale || p.State.Tradable || p.Version != 3 {
		t.Fatalf("after silence: %+v", p.State)
	}
	last := f.j.Records()[f.j.Len()-1]
	if last.Kind != journal.KindHealth || !strings.HasPrefix(last.Summary, "Feed stale") || !strings.Contains(last.Summary, "trading locked: feed stale") {
		t.Fatalf("health record %+v", last)
	}

	// Further ticks while stale publish nothing new.
	f.clk.Advance(time.Second)
	f.b.Tick()
	if f.current(t).Version != 3 {
		t.Fatal("stale market re-published without a change")
	}

	f.submit(t, specFeed[1])
	if p := f.current(t); p.State.Feed != market.FeedLive || !p.State.Tradable {
		t.Fatalf("after new event: %+v", p.State)
	}
}

func TestRestartRebuildsIdenticalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := journal.Open(path, discard)
	f := newFixture(t, j, nil)
	for _, raw := range specFeed {
		f.submit(t, raw)
		f.clk.Advance(time.Second)
	}
	f.submit(t, specFeed[0])                                                          // duplicate
	f.submit(t, `{"matchId":"match-1","marketId":"winner","seq":50,"status":"open"}`) // gap
	f.submit(t, `not json`)
	f.clk.Advance(10 * time.Second)
	f.b.Tick()
	f.submit(t, `{"matchId":"match-1","marketId":"winner","seq":52,"status":"suspended"}`)
	f.b.SubmitSnapshot([]byte(`{"matchId":"match-1","marketId":"winner","seq":52,"status":"suspended","odds":{"falcons":1.70,"wolves":2.20}}`), SourceRecovery)
	want := f.current(t)
	f.b.Close()
	j.Close()

	j2, err := journal.Open(path, discard)
	if err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, j2, nil)
	got := f2.current(t)
	if got.Epoch != want.Epoch || got.Version != want.Version || !bytes.Equal(got.JSON, want.JSON) {
		t.Fatalf("restored state differs:\n got %s\nwant %s", got.JSON, want.JSON)
	}
	if rep := f2.b.Verify(); !rep.ChainOK || !rep.ReplayOK {
		t.Fatalf("verify: %+v", rep)
	}

	// The restored board carries on from where it stopped.
	f2.clk.Advance(20 * time.Second)
	ack := f2.submit(t, `{"matchId":"match-1","marketId":"winner","seq":53,"status":"open"}`)
	if ack.Verdict != market.VerdictApplied || ack.Version <= want.Version {
		t.Fatalf("after restart: %+v", ack)
	}
	if p := f2.current(t); !p.State.Tradable || price(p, "falcons") != "1.70" {
		t.Fatalf("after restart: %+v", p.State)
	}
}

func TestRestoreRejectsAJournalItCannotReproduce(t *testing.T) {
	j := journal.NewMemory()
	f := newFixture(t, j, nil)
	f.submit(t, specFeed[0])

	// A record with a valid hash chain whose recorded state does not follow
	// from its payload (e.g. written by a buggy or different build).
	forged := f.j.Records()[1]
	forged.Payload = specFeed[1]
	forged.Seq = 42
	if _, err := j.Append(forged); err != nil {
		t.Fatal(err)
	}
	_, err := New([]market.Definition{def}, Config{Log: discard}, j, nil)
	if err == nil || !strings.Contains(err.Error(), "differs from the recorded one") {
		t.Fatalf("New accepted an irreproducible journal: %v", err)
	}
	if rep := f.b.Verify(); rep.ReplayOK || !rep.ChainOK {
		t.Fatalf("Verify did not flag the forged record: %+v", rep)
	}
}

type fakeSource struct {
	mu    sync.Mutex
	calls int
	raw   string
	err   error
	done  chan struct{}
}

func (s *fakeSource) Snapshot(ctx context.Context, k market.Key) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	defer func() { s.done <- struct{}{} }()
	return []byte(s.raw), s.err
}

func (s *fakeSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func waitDone(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot source not called")
	}
}

// waitIdle waits for an in-flight resync to finish applying.
func waitIdle(b *Board) {
	for {
		b.mu.Lock()
		busy := b.slots[key].recovering
		b.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGapTriggersSnapshotRecovery(t *testing.T) {
	src := &fakeSource{
		raw:  `{"matchId":"match-1","marketId":"winner","seq":45,"status":"open","odds":{"falcons":1.90,"wolves":1.95}}`,
		done: make(chan struct{}, 4),
	}
	f := newFixture(t, nil, src)
	f.submit(t, specFeed[0])
	f.submit(t, specFeed[4]) // 42-44 lost
	if f.current(t).State.Tradable {
		t.Fatal("tradable with a gap")
	}

	f.clk.Advance(time.Second)
	f.b.Tick()
	if src.Calls() != 0 {
		t.Fatal("recovery started before RecoverAfter; late messages deserve a chance")
	}

	f.clk.Advance(1500 * time.Millisecond)
	f.b.Tick()
	waitDone(t, src.done)
	waitIdle(f.b)

	p := f.current(t)
	if !p.State.Consistent || !p.State.Tradable || price(p, "falcons") != "1.90" {
		t.Fatalf("after recovery: %+v", p.State)
	}
	last := f.j.Records()[f.j.Len()-1]
	if last.Kind != journal.KindSnapshot || last.Source != string(SourceRecovery) || last.Payload != src.raw {
		t.Fatalf("recovery record %+v", last)
	}
	f.b.Tick()
	if src.Calls() != 1 {
		t.Fatal("recovery repeated after the market was consistent again")
	}
}

func TestFailedRecoveryIsJournaledAndRetried(t *testing.T) {
	src := &fakeSource{err: errors.New("provider unreachable"), done: make(chan struct{}, 4)}
	f := newFixture(t, nil, src)
	f.submit(t, specFeed[1]) // joined mid-stream: incomplete

	f.clk.Advance(2 * time.Second)
	f.b.Tick()
	waitDone(t, src.done)
	waitIdle(f.b)
	last := f.j.Records()[f.j.Len()-1]
	if last.Kind != journal.KindRecovery || !strings.Contains(last.Summary, "provider unreachable") {
		t.Fatalf("failure record %+v", last)
	}

	f.clk.Advance(2 * time.Second)
	f.b.Tick()
	if src.Calls() != 1 {
		t.Fatal("retried before RecoverEvery")
	}
	src.mu.Lock()
	src.err = nil
	src.raw = `{"matchId":"match-1","marketId":"winner","seq":42,"status":"open","odds":{"falcons":1.80,"wolves":2.05}}`
	src.mu.Unlock()
	f.clk.Advance(3 * time.Second)
	f.b.Tick()
	waitDone(t, src.done)
	waitIdle(f.b)
	if p := f.current(t); !p.State.Consistent {
		t.Fatalf("second attempt did not recover: %+v", p.State)
	}
}

func TestSubscribersCoalesceToLatest(t *testing.T) {
	f := newFixture(t, nil, nil)
	ch, cancel, ok := f.b.Subscribe(key)
	if !ok {
		t.Fatal("subscribe failed")
	}
	defer cancel()
	for _, raw := range specFeed {
		f.submit(t, raw)
	}
	select {
	case <-ch:
	default:
		t.Fatal("no notification")
	}
	select {
	case <-ch:
		t.Fatal("notifications did not coalesce")
	default:
	}
	if p := f.current(t); p.State.Seq != 45 {
		t.Fatalf("latest state seq %d", p.State.Seq)
	}
	if _, _, ok := f.b.Subscribe(market.Key{MatchID: "x", MarketID: "y"}); ok {
		t.Fatal("subscribed to an unknown market")
	}
}

func TestEverythingReceivedIsJournaled(t *testing.T) {
	f := newFixture(t, nil, nil)
	cases := []struct{ raw, reason string }{
		{`garbage`, "must be a JSON object"},
		{`{"matchId":"match-9","marketId":"winner","seq":1,"status":"open"}`, "unknown market match-9/winner"},
		{`{"seq":1}`, "matchId is required"},
		{`{"matchId":"match-1","marketId":"winner","seq":7,"status":"open","Status":"suspended"}`, "malformed"},
	}
	for _, c := range cases {
		ack := f.submit(t, c.raw)
		if ack.Verdict != market.VerdictRejected || !strings.Contains(ack.Reason, c.reason) {
			t.Errorf("%s: ack %+v, want reason containing %q", c.raw, ack, c.reason)
		}
		last := f.j.Records()[f.j.Len()-1]
		if last.Payload != c.raw || last.Verdict != market.VerdictRejected {
			t.Errorf("%s: not journaled verbatim: %+v", c.raw, last)
		}
	}
	// A rejected message for a known market is filed under it.
	last := f.j.Records()[f.j.Len()-1]
	if last.Key() != key || last.Seq != 7 || last.State == nil {
		t.Errorf("rejected known-market record %+v", last)
	}

	if _, err := f.b.SubmitBatch([]byte(`[{"seq":1},`), SourceFeed); err == nil {
		t.Fatal("broken batch accepted")
	}
	if last := f.j.Records()[f.j.Len()-1]; last.Payload != `[{"seq":1},` {
		t.Fatalf("broken batch not journaled: %+v", last)
	}
}

func TestSubmitBatch(t *testing.T) {
	f := newFixture(t, nil, nil)
	acks, err := f.b.SubmitBatch([]byte("["+strings.Join(specFeed, ",")+"]"), SourceReplay)
	if err != nil || len(acks) != 5 {
		t.Fatalf("batch: %v %v", acks, err)
	}
	for _, a := range acks {
		if a.Verdict != market.VerdictApplied {
			t.Fatalf("ack %+v", a)
		}
	}
	big := "[" + strings.Repeat(specFeed[0]+",", MaxBatch) + specFeed[0] + "]"
	if _, err := f.b.SubmitBatch([]byte(big), SourceFeed); err == nil {
		t.Fatal("oversized batch accepted")
	}
}

func TestResetStartsNewEpoch(t *testing.T) {
	f := newFixture(t, nil, nil)
	for _, raw := range specFeed {
		f.submit(t, raw)
	}
	before := f.current(t)
	p, err := f.b.Reset(key, SourceAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if p.Epoch == before.Epoch || p.Version <= before.Version || p.State.Seq != 0 || p.State.Tradable {
		t.Fatalf("after reset: %+v", p)
	}
	// The same feed can now be replayed from scratch.
	if ack := f.submit(t, specFeed[0]); ack.Verdict != market.VerdictApplied {
		t.Fatalf("replay after reset: %+v", ack)
	}
	if rep := f.b.Verify(); !rep.ReplayOK {
		t.Fatalf("verify after reset: %+v", rep)
	}
}

func TestStateAt(t *testing.T) {
	f := newFixture(t, nil, nil)
	for _, raw := range specFeed {
		f.clk.Advance(time.Second)
		f.submit(t, raw)
	}
	// A bet placed 2.5s in saw seq 42: open, Falcons 1.80.
	r, ok := f.b.StateAt(key, t0.Add(2500*time.Millisecond))
	if !ok || r.State.Seq != 42 || !r.State.Tradable {
		t.Fatalf("StateAt: %+v", r)
	}
	if p, _ := r.State.Price("falcons"); p.String() != "1.80" {
		t.Fatalf("falcons at bet time = %s", p)
	}
	// At 3.2s the market was suspended.
	if r, _ := f.b.StateAt(key, t0.Add(3200*time.Millisecond)); r.State.Status != market.StatusSuspended || r.State.Tradable {
		t.Fatalf("StateAt 3.2s: %+v", r.State)
	}
}

func TestHistoryPaging(t *testing.T) {
	f := newFixture(t, nil, nil)
	for _, raw := range specFeed {
		f.submit(t, raw)
	}
	f.submit(t, `{"matchId":"match-9","marketId":"winner","seq":1}`) // other market
	page := f.b.History(key, 0, 3)
	if len(page) != 3 || page[0].ID != 1 {
		t.Fatalf("page 1: %d records", len(page))
	}
	rest := f.b.History(key, page[2].ID, 100)
	if len(rest) != 3 {
		t.Fatalf("page 2: %d records", len(rest))
	}
}

// TestConcurrentUse exercises the board under the race detector and checks
// that the outcome matches sequential processing.
func TestConcurrentUse(t *testing.T) {
	f := newFixture(t, nil, nil)
	var events []string
	events = append(events, specFeed[0])
	for i := range 200 {
		events = append(events, fmt.Sprintf(`{"matchId":"match-1","marketId":"winner","seq":%d,"odds":{"falcons":%d.%02d}}`, 42+i, 1+i%5, 10+i%80))
	}

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := w; i < len(events); i += 4 {
				if _, err := f.b.SubmitEvent([]byte(events[i]), SourceFeed); err != nil {
					t.Error(err)
				}
				if _, err := f.b.SubmitEvent([]byte(events[i]), SourceFeed); err != nil { // duplicate
					t.Error(err)
				}
			}
		})
	}
	stop := make(chan struct{})
	for range 3 {
		wg.Go(func() {
			ch, cancel, _ := f.b.Subscribe(key)
			defer cancel()
			var last uint64
			for {
				select {
				case <-stop:
					return
				case <-ch:
					p := f.current(t)
					if p.Version < last {
						t.Errorf("version went backwards: %d after %d", p.Version, last)
					}
					last = p.Version
				}
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			f.b.Tick()
			f.b.History(key, 0, 10)
		}
	})
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	seq := newFixture(t, nil, nil)
	for _, raw := range events {
		seq.submit(t, raw)
	}
	got, want := f.current(t).State, seq.current(t).State
	if got.Seq != want.Seq || !got.Consistent || price(f.current(t), "falcons") != price(seq.current(t), "falcons") {
		t.Fatalf("concurrent result differs: %+v vs %+v", got, want)
	}
	if rep := f.b.Verify(); !rep.ChainOK || !rep.ReplayOK {
		t.Fatalf("verify: %+v", rep)
	}
}
