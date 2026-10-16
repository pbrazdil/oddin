package market

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	testKey = Key{MatchID: "match-1", MarketID: "winner"}
	testDef = Definition{
		Key:   testKey,
		Event: "Falcons vs Wolves",
		Name:  "Match winner",
		Outcomes: []OutcomeDef{
			{ID: "falcons", Name: "Falcons"},
			{ID: "wolves", Name: "Wolves"},
		},
	}
	t0 = time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
)

const staleAfter = 3 * time.Second

// specFeed is the example input from the brief.
var specFeed = []string{
	`{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`,
	`{"matchId":"match-1","marketId":"winner","seq":42,"odds":{"falcons":1.80}}`,
	`{"matchId":"match-1","marketId":"winner","seq":43,"status":"suspended"}`,
	`{"matchId":"match-1","marketId":"winner","seq":44,"status":"open"}`,
	`{"matchId":"match-1","marketId":"winner","seq":45,"odds":{"falcons":1.90,"wolves":1.95}}`,
}

func newTestMarket() *Market { return New(testDef, Config{StaleAfter: staleAfter}) }

// ev builds an event from compact notation: ev(42, "", "falcons=1.80").
func ev(seq uint64, status Status, odds ...string) Event {
	e := Event{Key: testKey, Seq: seq, Status: status}
	for _, kv := range odds {
		name, lit, _ := strings.Cut(kv, "=")
		if e.Odds == nil {
			e.Odds = map[Outcome]Price{}
		}
		e.Odds[Outcome(name)] = MustPrice(lit)
	}
	return e
}

func snap(seq uint64, status Status, falcons, wolves string) Snapshot {
	return Snapshot{Key: testKey, Seq: seq, Status: status, Odds: map[Outcome]Price{
		"falcons": MustPrice(falcons), "wolves": MustPrice(wolves),
	}}
}

func applyRaw(t *testing.T, m *Market, raw string, at time.Time) Result {
	t.Helper()
	e, err := ParseEvent([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEvent(%s): %v", raw, err)
	}
	return m.ApplyEvent(e, at)
}

func priceOf(t *testing.T, s State, o Outcome) string {
	t.Helper()
	p, ok := s.Price(o)
	if !ok {
		return "-"
	}
	return p.String()
}

func outcomeOf(s State, o Outcome) OutcomeState {
	for _, os := range s.Outcomes {
		if os.ID == o {
			return os
		}
	}
	return OutcomeState{}
}

// expect checks the parts of a state every scenario cares about.
type expect struct {
	status   Status
	falcons  string
	wolves   string
	tradable bool
	reasons  []Reason
}

func checkState(t *testing.T, step string, s State, want expect) {
	t.Helper()
	if s.Status != want.status {
		t.Errorf("%s: status = %s, want %s", step, s.Status, want.status)
	}
	if got := priceOf(t, s, "falcons"); got != want.falcons {
		t.Errorf("%s: falcons = %s, want %s", step, got, want.falcons)
	}
	if got := priceOf(t, s, "wolves"); got != want.wolves {
		t.Errorf("%s: wolves = %s, want %s", step, got, want.wolves)
	}
	if s.Tradable != want.tradable {
		t.Errorf("%s: tradable = %v, want %v (reasons %v)", step, s.Tradable, want.tradable, s.Reasons)
	}
	if want.reasons == nil {
		want.reasons = []Reason{}
	}
	if !slices.Equal(s.Reasons, want.reasons) {
		t.Errorf("%s: reasons = %v, want %v", step, s.Reasons, want.reasons)
	}
}

func TestSpecExampleInOrder(t *testing.T) {
	m := newTestMarket()
	steps := []expect{
		{StatusOpen, "1.85", "2.05", true, nil},
		{StatusOpen, "1.80", "2.05", true, nil},
		{StatusSuspended, "1.80", "2.05", false, []Reason{ReasonSuspended}},
		{StatusOpen, "1.80", "2.05", true, nil},
		{StatusOpen, "1.90", "1.95", true, nil},
	}
	for i, raw := range specFeed {
		at := t0.Add(time.Duration(i) * time.Second)
		r := applyRaw(t, m, raw, at)
		if r.Verdict != VerdictApplied {
			t.Fatalf("seq %d: verdict %s (%s)", r.Seq, r.Verdict, r.Reason)
		}
		s := m.State(at)
		checkState(t, fmt.Sprintf("after seq %d", r.Seq), s, steps[i])
		if !s.Consistent || s.Seq != r.Seq || s.Feed != FeedLive {
			t.Errorf("seq %d: consistent=%v seq=%d feed=%s", r.Seq, s.Consistent, s.Seq, s.Feed)
		}
	}
}

func TestSpecExampleChangesAndMovement(t *testing.T) {
	m := newTestMarket()
	var changes [][]Change
	for i, raw := range specFeed {
		changes = append(changes, applyRaw(t, m, raw, t0.Add(time.Duration(i)*time.Second)).Changes)
	}
	want := [][]Change{
		{{Field: "status", To: "open"}, {Field: "odds.falcons", To: "1.85"}, {Field: "odds.wolves", To: "2.05"}},
		{{Field: "odds.falcons", From: "1.85", To: "1.80"}},
		{{Field: "status", From: "open", To: "suspended"}},
		{{Field: "status", From: "suspended", To: "open"}},
		{{Field: "odds.falcons", From: "1.80", To: "1.90"}, {Field: "odds.wolves", From: "2.05", To: "1.95"}},
	}
	for i := range want {
		if !slices.Equal(changes[i], want[i]) {
			t.Errorf("seq %d changes = %v, want %v", 41+i, changes[i], want[i])
		}
	}
	s := m.State(t0.Add(4 * time.Second))
	f, w := outcomeOf(s, "falcons"), outcomeOf(s, "wolves")
	if f.Move != MoveUp || f.Prev.String() != "1.80" || f.Seq != 45 {
		t.Errorf("falcons = %+v, want up from 1.80 at seq 45", f)
	}
	if w.Move != MoveDown || w.Prev.String() != "2.05" || w.Seq != 45 {
		t.Errorf("wolves = %+v, want down from 2.05 at seq 45", w)
	}
	if !w.ChangedAt.Equal(t0.Add(4 * time.Second)) {
		t.Errorf("wolves changedAt = %v", w.ChangedAt)
	}
}

func TestPricesAndAvailabilityAreIndependent(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(1, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)

	// A status-only event must not touch prices.
	m.ApplyEvent(ev(2, StatusSuspended), t0)
	checkState(t, "status only", m.State(t0), expect{StatusSuspended, "1.85", "2.05", false, []Reason{ReasonSuspended}})

	// Prices may move while suspended; the market stays suspended.
	m.ApplyEvent(ev(3, "", "wolves=2.10"), t0)
	checkState(t, "odds while suspended", m.State(t0), expect{StatusSuspended, "1.85", "2.10", false, []Reason{ReasonSuspended}})

	// A price-only event must not touch the status.
	m.ApplyEvent(ev(4, StatusOpen), t0)
	m.ApplyEvent(ev(5, "", "falcons=1.70"), t0)
	checkState(t, "odds only", m.State(t0), expect{StatusOpen, "1.70", "2.10", true, nil})

	// An empty event only advances the sequence.
	r := m.ApplyEvent(ev(6, ""), t0)
	if r.Verdict != VerdictApplied || len(r.Changes) != 0 {
		t.Fatalf("empty event: %+v", r)
	}
	checkState(t, "empty", m.State(t0), expect{StatusOpen, "1.70", "2.10", true, nil})
}

func TestClosedIsNotTradable(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(1, StatusClosed, "falcons=1.85", "wolves=2.05"), t0)
	checkState(t, "closed", m.State(t0), expect{StatusClosed, "1.85", "2.05", false, []Reason{ReasonClosed}})
}

func TestNoDataYet(t *testing.T) {
	s := newTestMarket().State(t0)
	checkState(t, "empty market", s, expect{StatusUnknown, "-", "-", false, []Reason{ReasonNoData}})
	if s.Feed != FeedWaiting || s.Consistent || s.Seq != 0 {
		t.Errorf("empty market: %+v", s)
	}
	for _, o := range s.Outcomes {
		if o.Price != nil {
			t.Errorf("outcome %s has a price before any data", o.ID)
		}
	}
}

func TestDuplicatesAreIdempotent(t *testing.T) {
	m := newTestMarket()
	for i, raw := range specFeed {
		applyRaw(t, m, raw, t0.Add(time.Duration(i)*time.Second))
	}
	at := t0.Add(4 * time.Second)
	before := mustJSON(t, m.State(at))
	for _, raw := range specFeed {
		r := applyRaw(t, m, raw, at)
		if r.Verdict != VerdictDuplicate || r.Committable() {
			t.Fatalf("re-delivery of seq %d: %+v", r.Seq, r)
		}
	}
	if after := mustJSON(t, m.State(at)); after != before {
		t.Fatalf("duplicates changed the state:\n%s\n%s", before, after)
	}
}

func TestDuplicateWithDifferentEncodingIsStillDuplicate(t *testing.T) {
	m := newTestMarket()
	applyRaw(t, m, `{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`, t0)
	r := applyRaw(t, m, `{"odds":{"wolves":2.050,"falcons":185e-2},"status":"open","seq":41,"marketId":"winner","matchId":"match-1"}`, t0)
	if r.Verdict != VerdictDuplicate {
		t.Fatalf("verdict %s, want duplicate", r.Verdict)
	}
}

func TestConflictingDuplicateLocksMarket(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(42, "", "falcons=1.80"), t0)

	r := m.ApplyEvent(ev(42, "", "falcons=1.70"), t0)
	if r.Verdict != VerdictConflict {
		t.Fatalf("verdict %s, want conflict", r.Verdict)
	}
	if len(r.Changes) != 0 {
		t.Fatalf("conflict applied changes: %v", r.Changes)
	}
	s := m.State(t0)
	// The first value is kept, but nothing about seq 42 can be trusted.
	checkState(t, "after conflict", s, expect{StatusOpen, "1.80", "2.05", false, []Reason{ReasonConflict}})
	if !slices.Equal(s.Conflicts, Ranges{{42, 42}}) {
		t.Errorf("conflicts = %v", s.Conflicts)
	}

	// Re-proving only one field is not enough...
	m.ApplyEvent(ev(43, "", "falcons=1.75"), t0)
	if m.State(t0).Tradable {
		t.Fatal("tradable while status and wolves still date from before the conflict")
	}
	// ...once every field is restated after the conflict, it is irrelevant.
	m.ApplyEvent(ev(44, StatusOpen, "wolves=2.10"), t0)
	s = m.State(t0)
	checkState(t, "after restating", s, expect{StatusOpen, "1.75", "2.10", true, nil})
	if len(s.Conflicts) != 0 {
		t.Errorf("irrelevant conflicts still reported: %v", s.Conflicts)
	}
}

func TestConflictResolvedBySnapshot(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(42, StatusSuspended), t0)
	m.ApplyEvent(ev(42, StatusOpen), t0) // conflict
	if m.State(t0).Tradable {
		t.Fatal("tradable after conflict")
	}
	r := m.ApplySnapshot(snap(42, StatusSuspended, "1.85", "2.05"), t0)
	if r.Verdict != VerdictApplied {
		t.Fatalf("snapshot verdict %s: %s", r.Verdict, r.Reason)
	}
	checkState(t, "after snapshot", m.State(t0), expect{StatusSuspended, "1.85", "2.05", false, []Reason{ReasonSuspended}})
	if s := m.State(t0); !s.Consistent || len(s.Conflicts) != 0 {
		t.Fatalf("snapshot did not resolve the conflict: %+v", s)
	}
}

// TestMissedSuspensionNeverLooksTradable is the core money-safety case: the
// feed drops 42-44, including a suspension, and then sends a price-only
// update. The status we hold (open, from 41) may be wrong.
func TestMissedSuspensionNeverLooksTradable(t *testing.T) {
	m := newTestMarket()
	applyRaw(t, m, specFeed[0], t0)
	applyRaw(t, m, specFeed[4], t0.Add(time.Second))

	s := m.State(t0.Add(time.Second))
	checkState(t, "gap", s, expect{StatusOpen, "1.90", "1.95", false, []Reason{ReasonSequenceGap}})
	if s.Consistent || !slices.Equal(s.Gaps, Ranges{{42, 44}}) {
		t.Fatalf("gaps = %v consistent = %v", s.Gaps, s.Consistent)
	}

	// The suspension arrives late; status is now as of 43...
	r := applyRaw(t, m, specFeed[2], t0.Add(2*time.Second))
	if r.Verdict != VerdictLate {
		t.Fatalf("late verdict = %s", r.Verdict)
	}
	checkState(t, "late 43", m.State(t0.Add(2*time.Second)), expect{StatusSuspended, "1.90", "1.95", false, []Reason{ReasonSequenceGap, ReasonSuspended}})

	// ...the reopening arrives late too. 42 is still missing, but every
	// field has been stated after it, so it cannot matter any more.
	applyRaw(t, m, specFeed[3], t0.Add(2*time.Second))
	s = m.State(t0.Add(2 * time.Second))
	checkState(t, "late 44", s, expect{StatusOpen, "1.90", "1.95", true, nil})
	if !s.Consistent || len(s.Gaps) != 0 {
		t.Fatalf("expected consistent without seq 42: %+v", s)
	}

	// 42 finally arrives and changes nothing: falcons 1.90 at 45 is newer.
	r = applyRaw(t, m, specFeed[1], t0.Add(2*time.Second))
	if r.Verdict != VerdictLate || len(r.Changes) != 0 {
		t.Fatalf("seq 42: %+v", r)
	}
	checkState(t, "late 42", m.State(t0.Add(2*time.Second)), expect{StatusOpen, "1.90", "1.95", true, nil})
}

func TestGapBlocksOnlyUntilFilled(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(43, "", "falcons=1.95"), t0)
	s := m.State(t0)
	checkState(t, "gap", s, expect{StatusOpen, "1.95", "2.05", false, []Reason{ReasonSequenceGap}})

	r := m.ApplyEvent(ev(42, "", "falcons=1.80", "wolves=2.10"), t0)
	if r.Verdict != VerdictLate {
		t.Fatalf("verdict %s", r.Verdict)
	}
	// falcons keeps 1.95 from seq 43; wolves takes 2.10 from seq 42.
	if !slices.Equal(r.Changes, []Change{{Field: "odds.wolves", From: "2.05", To: "2.10"}}) {
		t.Fatalf("changes = %v", r.Changes)
	}
	checkState(t, "filled", m.State(t0), expect{StatusOpen, "1.95", "2.10", true, nil})
}

func TestLateEventNeverOverwritesNewerValue(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(10, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(12, StatusSuspended, "falcons=1.95"), t0)
	m.ApplyEvent(ev(11, StatusOpen, "falcons=1.50", "wolves=2.50"), t0)
	s := m.State(t0)
	checkState(t, "after late 11", s, expect{StatusSuspended, "1.95", "2.50", false, []Reason{ReasonSuspended}})
	if s.StatusSeq != 12 || outcomeOf(s, "falcons").Seq != 12 || outcomeOf(s, "wolves").Seq != 11 {
		t.Errorf("field seqs: status %d, falcons %d, wolves %d", s.StatusSeq, outcomeOf(s, "falcons").Seq, outcomeOf(s, "wolves").Seq)
	}
}

func TestStartingMidStream(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(42, "", "falcons=1.80"), t0)
	s := m.State(t0)
	checkState(t, "partial first", s, expect{StatusUnknown, "1.80", "-", false, []Reason{ReasonIncomplete, ReasonSequenceGap}})

	m.ApplyEvent(ev(43, StatusSuspended), t0)
	checkState(t, "status", m.State(t0), expect{StatusSuspended, "1.80", "-", false, []Reason{ReasonIncomplete, ReasonSequenceGap, ReasonSuspended}})

	m.ApplyEvent(ev(45, "", "falcons=1.90", "wolves=1.95"), t0)
	s = m.State(t0)
	// Everything is known, but 44 may have changed the status after 43.
	checkState(t, "complete with gap", s, expect{StatusSuspended, "1.90", "1.95", false, []Reason{ReasonSequenceGap, ReasonSuspended}})
	if !slices.Equal(s.Gaps, Ranges{{44, 44}}) {
		t.Errorf("gaps = %v", s.Gaps)
	}

	m.ApplyEvent(ev(44, StatusOpen), t0)
	checkState(t, "filled", m.State(t0), expect{StatusOpen, "1.90", "1.95", true, nil})
}

func TestFirstFullEventIsEnough(t *testing.T) {
	m := newTestMarket()
	applyRaw(t, m, specFeed[0], t0)
	s := m.State(t0)
	if !s.Consistent || !s.Tradable || len(s.Gaps) != 0 {
		t.Fatalf("seqs 1-40 must not matter after a full event at 41: %+v", s)
	}

	// An event from before we joined changes nothing.
	r := m.ApplyEvent(ev(39, StatusSuspended, "falcons=1.50"), t0)
	if r.Verdict != VerdictLate || len(r.Changes) != 0 {
		t.Fatalf("seq 39: %+v", r)
	}
	if !m.State(t0).Tradable {
		t.Fatal("old event made the market untradable")
	}
}

func TestRestatingSameValuesProvesState(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(43, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	s := m.State(t0)
	if !s.Tradable {
		t.Fatalf("a full restatement after the gap should prove the state: %+v", s)
	}
}

func TestRejectedEventsChangeNothing(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	before := mustJSON(t, m.State(t0))

	bad := []Event{
		// One valid and one unknown outcome: nothing is applied.
		{Key: testKey, Seq: 42, Odds: map[Outcome]Price{"falcons": 18000, "draw": 30000}},
		{Key: Key{"match-2", "winner"}, Seq: 42, Status: StatusSuspended},
		{Key: Key{"match-1", "handicap"}, Seq: 42, Status: StatusSuspended},
		{Key: testKey, Seq: 42, Status: "paused"},
		{Key: testKey, Seq: 42, Status: StatusUnknown},
		{Key: testKey, Seq: 42, Odds: map[Outcome]Price{"falcons": 10000}},
		{Key: testKey, Seq: 42, Odds: map[Outcome]Price{"falcons": MaxPrice + 1}},
		{Key: testKey, Seq: 0, Status: StatusSuspended},
		{Key: testKey, Seq: MaxSeq + 1, Status: StatusSuspended},
	}
	for _, e := range bad {
		r := m.ApplyEvent(e, t0)
		if r.Verdict != VerdictRejected || r.Committable() || r.Reason == "" {
			t.Errorf("event %+v: %+v", e, r)
		}
	}
	if after := mustJSON(t, m.State(t0)); after != before {
		t.Fatalf("rejected events changed the state")
	}

	// A rejected seq was not consumed: a valid event may still use it.
	if r := m.ApplyEvent(ev(42, "", "falcons=1.80"), t0); r.Verdict != VerdictApplied {
		t.Fatalf("valid seq 42 after rejections: %s", r.Verdict)
	}
}

func TestRejectedSeqLeavesAGap(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(Event{Key: testKey, Seq: 42, Odds: map[Outcome]Price{"draw": 30000}}, t0)
	m.ApplyEvent(ev(43, "", "falcons=1.80"), t0)
	// We do not know what 42 really said, so the board must not trust 41's
	// status and wolves price.
	checkState(t, "after rejected 42", m.State(t0), expect{StatusOpen, "1.80", "2.05", false, []Reason{ReasonSequenceGap}})
}

func TestSnapshotRecoversFromGap(t *testing.T) {
	m := newTestMarket()
	applyRaw(t, m, specFeed[0], t0)
	applyRaw(t, m, specFeed[4], t0)
	if m.State(t0).Tradable {
		t.Fatal("tradable with a gap")
	}
	r := m.ApplySnapshot(snap(45, StatusOpen, "1.90", "1.95"), t0)
	if r.Verdict != VerdictApplied || r.Progress {
		t.Fatalf("snapshot at head: %+v", r)
	}
	s := m.State(t0)
	checkState(t, "after snapshot", s, expect{StatusOpen, "1.90", "1.95", true, nil})
	if s.Seq != 45 || len(s.Gaps) != 0 {
		t.Fatalf("seq %d gaps %v", s.Seq, s.Gaps)
	}

	// Seqs the snapshot covered may still trickle in; they change nothing.
	r = applyRaw(t, m, specFeed[2], t0)
	if r.Verdict != VerdictLate || len(r.Changes) != 0 {
		t.Fatalf("covered seq 43: %+v", r)
	}
	checkState(t, "after covered 43", m.State(t0), expect{StatusOpen, "1.90", "1.95", true, nil})
}

func TestSnapshotAheadOfHead(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	r := m.ApplySnapshot(snap(50, StatusSuspended, "1.70", "2.20"), t0.Add(time.Second))
	if !r.Progress {
		t.Fatal("snapshot ahead of head must count as progress")
	}
	s := m.State(t0.Add(time.Second))
	checkState(t, "snapshot ahead", s, expect{StatusSuspended, "1.70", "2.20", false, []Reason{ReasonSuspended}})
	if s.Seq != 50 || !s.Consistent || !s.LastEventAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("state %+v", s)
	}
	// The next delta continues from the snapshot without a gap.
	m.ApplyEvent(ev(51, StatusOpen), t0)
	checkState(t, "after 51", m.State(t0.Add(time.Second)), expect{StatusOpen, "1.70", "2.20", true, nil})
}

func TestSnapshotKeepsNewerFields(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(43, "", "falcons=1.95"), t0)
	r := m.ApplySnapshot(snap(42, StatusOpen, "1.80", "2.10"), t0)
	if r.Verdict != VerdictApplied {
		t.Fatalf("verdict %s: %s", r.Verdict, r.Reason)
	}
	s := m.State(t0)
	checkState(t, "older snapshot", s, expect{StatusOpen, "1.95", "2.10", true, nil})
	if outcomeOf(s, "falcons").Seq != 43 {
		t.Error("snapshot overwrote a newer field")
	}
}

func TestSnapshotContradictingEventIsConflict(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(42, "", "falcons=1.80"), t0)
	before := mustJSON(t, m.State(t0))

	r := m.ApplySnapshot(snap(42, StatusOpen, "1.70", "2.05"), t0)
	if r.Verdict != VerdictConflict || len(r.Changes) != 0 {
		t.Fatalf("snapshot contradicting seq 42: %+v", r)
	}
	s := m.State(t0)
	checkState(t, "after contradicting snapshot", s, expect{StatusOpen, "1.80", "2.05", false, []Reason{ReasonConflict}})
	if mustJSON(t, s) == before {
		t.Fatal("conflict was not recorded")
	}

	// A newer snapshot settles it.
	m.ApplySnapshot(snap(43, StatusOpen, "1.70", "2.05"), t0)
	checkState(t, "after newer snapshot", m.State(t0), expect{StatusOpen, "1.70", "2.05", true, nil})
}

func TestSnapshotContradictingStatusIsConflict(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(42, StatusSuspended), t0)
	if r := m.ApplySnapshot(snap(42, StatusOpen, "1.85", "2.05"), t0); r.Verdict != VerdictConflict {
		t.Fatalf("snapshot reopening a market suspended at the same seq: %s", r.Verdict)
	}
	checkState(t, "status conflict", m.State(t0), expect{StatusSuspended, "1.85", "2.05", false, []Reason{ReasonConflict, ReasonSuspended}})

	m2 := newTestMarket()
	m2.ApplySnapshot(snap(42, StatusSuspended, "1.85", "2.05"), t0)
	if r := m2.ApplyEvent(ev(42, StatusOpen), t0); r.Verdict != VerdictConflict {
		t.Fatalf("late event reopening a market the snapshot says is suspended: %s", r.Verdict)
	}
	checkState(t, "late status conflict", m2.State(t0), expect{StatusSuspended, "1.85", "2.05", false, []Reason{ReasonConflict, ReasonSuspended}})
}

func TestLateEventContradictingSnapshot(t *testing.T) {
	m := newTestMarket()
	m.ApplySnapshot(snap(50, StatusOpen, "1.80", "2.05"), t0)
	if r := m.ApplyEvent(ev(50, "", "falcons=1.80"), t0); r.Verdict != VerdictLate {
		t.Fatalf("agreeing event: %s", r.Verdict)
	}
	if !m.State(t0).Tradable {
		t.Fatal("agreeing event broke the market")
	}
	m2 := newTestMarket()
	m2.ApplySnapshot(snap(50, StatusOpen, "1.80", "2.05"), t0)
	if r := m2.ApplyEvent(ev(50, "", "falcons=1.70"), t0); r.Verdict != VerdictConflict {
		t.Fatalf("contradicting event: %s", r.Verdict)
	}
	checkState(t, "contradicting event", m2.State(t0), expect{StatusOpen, "1.80", "2.05", false, []Reason{ReasonConflict}})
}

func TestSnapshotMustBeComplete(t *testing.T) {
	m := newTestMarket()
	cases := []Snapshot{
		{Key: testKey, Seq: 5, Status: StatusOpen, Odds: map[Outcome]Price{"falcons": 18500}},
		{Key: testKey, Seq: 5, Odds: map[Outcome]Price{"falcons": 18500, "wolves": 20500}},
		{Key: testKey, Seq: 5, Status: StatusOpen, Odds: map[Outcome]Price{"falcons": 18500, "wolves": 20500, "draw": 30000}},
		{Key: Key{"match-2", "winner"}, Seq: 5, Status: StatusOpen, Odds: map[Outcome]Price{"falcons": 18500, "wolves": 20500}},
	}
	for _, s := range cases {
		if r := m.ApplySnapshot(s, t0); r.Verdict != VerdictRejected {
			t.Errorf("snapshot %+v: verdict %s", s, r.Verdict)
		}
	}
	if m.State(t0).Seq != 0 {
		t.Fatal("rejected snapshots changed the market")
	}
}

func TestFeedStaleness(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)

	if s := m.State(t0.Add(staleAfter)); s.Feed != FeedLive || !s.Tradable {
		t.Fatalf("at exactly StaleAfter: %s", s.Feed)
	}
	stale := t0.Add(staleAfter + time.Millisecond)
	s := m.State(stale)
	checkState(t, "stale", s, expect{StatusOpen, "1.85", "2.05", false, []Reason{ReasonFeedStale}})
	if s.Feed != FeedStale || !s.Consistent {
		t.Fatalf("stale state: %+v", s)
	}

	// Re-deliveries, conflicts and late fills are not signs of a live feed.
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), stale)
	m.ApplyEvent(ev(41, StatusSuspended), stale)
	m.ApplyEvent(ev(40, ""), stale)
	if s := m.State(stale); s.Feed != FeedStale {
		t.Fatalf("non-progress messages refreshed the feed: %s", s.Feed)
	}

	m.ApplyEvent(ev(42, ""), stale)
	if s := m.State(stale); s.Feed != FeedLive || !s.LastEventAt.Equal(stale) {
		t.Fatalf("new seq did not refresh the feed: %+v", s)
	}
}

func TestHugeSeqJumpFailsClosed(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	m.ApplyEvent(ev(MaxSeq, "", "falcons=1.80"), t0)
	s := m.State(t0)
	if s.Tradable || !slices.Equal(s.Gaps, Ranges{{42, MaxSeq - 1}}) {
		t.Fatalf("huge jump: tradable=%v gaps=%v", s.Tradable, s.Gaps)
	}
	// Normal-looking events below the bogus head are late and cannot prove
	// the state on their own.
	m.ApplyEvent(ev(42, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	if m.State(t0).Tradable {
		t.Fatal("tradable below a bogus head")
	}
	m.ApplySnapshot(snap(MaxSeq, StatusOpen, "1.80", "2.05"), t0)
	if !m.State(t0).Tradable {
		t.Fatal("snapshot did not recover")
	}
}

func TestPlanDoesNotMutate(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	before := mustJSON(t, m.State(t0))

	plans := []Result{
		m.PlanEvent(ev(42, StatusSuspended, "falcons=1.80"), t0),
		m.PlanEvent(ev(45, "", "wolves=1.95"), t0),
		m.PlanEvent(ev(41, StatusClosed), t0),
		m.PlanSnapshot(snap(50, StatusClosed, "1.10", "9.00"), t0),
	}
	if after := mustJSON(t, m.State(t0)); after != before {
		t.Fatal("planning mutated the market")
	}
	preview := m.Preview(plans[0], t0)
	if preview.Status != StatusSuspended || priceOf(t, preview, "falcons") != "1.80" {
		t.Fatalf("preview = %+v", preview)
	}
	m.Commit(plans[0])
	if got := mustJSON(t, m.State(t0)); got != mustJSON(t, preview) {
		t.Fatal("committed state differs from preview")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("committing a stale plan did not panic")
		}
	}()
	m.Commit(plans[1])
}

func TestUncommittedPlansCanBeDropped(t *testing.T) {
	m := newTestMarket()
	m.ApplyEvent(ev(41, StatusOpen, "falcons=1.85", "wolves=2.05"), t0)
	_ = m.PlanEvent(ev(42, StatusSuspended), t0) // e.g. the journal write failed
	// The same event can be planned and committed again later.
	if r := m.ApplyEvent(ev(42, StatusSuspended), t0); r.Verdict != VerdictApplied {
		t.Fatalf("retry after dropped plan: %s", r.Verdict)
	}
}

func TestStateJSONShape(t *testing.T) {
	m := newTestMarket()
	applyRaw(t, m, specFeed[0], t0)
	applyRaw(t, m, specFeed[1], t0.Add(time.Second))
	got := mustJSON(t, m.State(t0.Add(time.Second)))
	for _, want := range []string{
		`"price":"1.80"`, `"prev":"1.85"`, `"move":"down"`, `"price":"2.05"`,
		`"status":"open"`, `"tradable":true`, `"consistent":true`, `"gaps":[]`, `"reasons":[]`,
		`"feed":"live"`, `"seq":42`, `"event":"Falcons vs Wolves"`, `"market":"Match winner"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("state JSON lacks %s:\n%s", want, got)
		}
	}
	empty := mustJSON(t, newTestMarket().State(t0))
	if !strings.Contains(empty, `"price":null`) || !strings.Contains(empty, `"status":"unknown"`) {
		t.Errorf("empty state JSON: %s", empty)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
