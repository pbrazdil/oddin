package journal

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oddsboard/internal/market"
)

var (
	t0      = time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func sample(i int) Record {
	p := market.MustPrice("1.85")
	return Record{
		At:       t0.Add(time.Duration(i) * time.Second),
		Kind:     KindEvent,
		Source:   "feed",
		MatchID:  "match-1",
		MarketID: "winner",
		Seq:      uint64(40 + i),
		Payload:  `{"matchId":"match-1","marketId":"winner","seq":41,"status":"open"}`,
		Verdict:  market.VerdictApplied,
		Changes:  []market.Change{{Field: "status", To: "open"}},
		Summary:  "Market opened",
		Epoch:    "e1",
		Version:  uint64(i),
		State: &market.State{
			MatchID: "match-1", MarketID: "winner", Seq: uint64(40 + i), Status: market.StatusOpen,
			Outcomes: []market.OutcomeState{{ID: "falcons", Name: "Falcons", Price: &p, Seq: 41}},
			Gaps:     market.Ranges{}, Reasons: []market.Reason{},
		},
	}
}

func openT(t *testing.T, path string) *Journal {
	t.Helper()
	j, err := Open(path, discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func fill(t *testing.T, j *Journal, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := j.Append(sample(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppendAssignsIDsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j := openT(t, path)
	fill(t, j, 3)
	head := j.Head()
	want := j.Records()
	j.Close()

	again := openT(t, path)
	got := again.Records()
	if len(got) != 3 {
		t.Fatalf("reloaded %d records", len(got))
	}
	for i := range got {
		if got[i].ID != uint64(i+1) {
			t.Errorf("record %d has id %d", i, got[i].ID)
		}
		if !got[i].At.Equal(want[i].At) || got[i].Summary != want[i].Summary || got[i].State.Seq != want[i].State.Seq {
			t.Errorf("record %d differs after reload: %+v vs %+v", i, got[i], want[i])
		}
		if p, _ := got[i].State.Price("falcons"); p.String() != "1.85" {
			t.Errorf("price lost in round trip: %v", p)
		}
	}
	if again.Head() != head {
		t.Error("head hash changed across reload")
	}
	// Appending continues the chain.
	if r, err := again.Append(sample(4)); err != nil || r.ID != 4 {
		t.Fatalf("append after reload: %v %v", r.ID, err)
	}
	if n, err := again.Verify(); err != nil || n != 4 {
		t.Fatalf("Verify = %d, %v", n, err)
	}
}

func TestMemoryJournal(t *testing.T) {
	j := NewMemory()
	fill(t, j, 5)
	if j.Len() != 5 {
		t.Fatalf("len %d", j.Len())
	}
	if n, err := j.Verify(); err != nil || n != 5 {
		t.Fatalf("Verify = %d, %v", n, err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	tamper := map[string]func(lines [][]byte) [][]byte{
		"edited price": func(l [][]byte) [][]byte {
			l[1] = bytes.Replace(l[1], []byte(`"1.85"`), []byte(`"1.95"`), 1)
			return l
		},
		"edited verdict": func(l [][]byte) [][]byte {
			l[0] = bytes.Replace(l[0], []byte(`"applied"`), []byte(`"rejected"`), 1)
			return l
		},
		"deleted line": func(l [][]byte) [][]byte { return append(l[:1], l[2:]...) },
		"swapped lines": func(l [][]byte) [][]byte {
			l[0], l[1] = l[1], l[0]
			return l
		},
		"edited last line": func(l [][]byte) [][]byte {
			n := len(l) - 1
			l[n] = bytes.Replace(l[n], []byte(`Market opened`), []byte(`Market closed`), 1)
			return l
		},
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			j := openT(t, path)
			fill(t, j, 3)
			j.Close()

			data, _ := os.ReadFile(path)
			lines := bytes.SplitAfter(data, []byte("\n"))
			lines = lines[:len(lines)-1] // trailing empty element
			if err := os.WriteFile(path, bytes.Join(mutate(lines), nil), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, discard); err == nil {
				t.Fatal("tampered journal was accepted")
			}
		})
	}
}

func TestTornFinalLineIsTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j := openT(t, path)
	fill(t, j, 2)
	j.Close()

	// Simulate a crash in the middle of writing record 3.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"prev":"abc","hash":"def","record":{"id":3,"summ`)
	f.Close()

	again := openT(t, path)
	if again.Len() != 2 {
		t.Fatalf("len %d after torn write", again.Len())
	}
	if r, err := again.Append(sample(3)); err != nil || r.ID != 3 {
		t.Fatalf("append after recovery: %d, %v", r.ID, err)
	}
	again.Close()
	final := openT(t, path)
	if final.Len() != 3 {
		t.Fatalf("len %d after recovery", final.Len())
	}
}

func TestVerifyDetectsChangesOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j := openT(t, path)
	fill(t, j, 3)
	data, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(data, []byte(`"1.85"`), []byte(`"1.11"`), 1), 0o600)
	if _, err := j.Verify(); err == nil {
		t.Fatal("Verify missed an on-disk modification")
	}
}

func TestFailedWriteBreaksJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j := openT(t, path)
	fill(t, j, 1)
	j.f.Close() // make the next write fail
	if _, err := j.Append(sample(2)); err == nil {
		t.Fatal("append to a closed file succeeded")
	}
	if _, err := j.Append(sample(3)); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("journal accepted appends after a failure: %v", err)
	}
	if j.Len() != 1 {
		t.Fatalf("failed appends were recorded: %d", j.Len())
	}
}

func TestQueryAndLastBefore(t *testing.T) {
	j := NewMemory()
	fill(t, j, 10)
	page := j.Query(3, 4, nil)
	if len(page) != 4 || page[0].ID != 4 || page[3].ID != 7 {
		t.Fatalf("Query page: %v", ids(page))
	}
	even := j.Query(0, 100, func(r Record) bool { return r.ID%2 == 0 })
	if len(even) != 5 || even[0].ID != 2 {
		t.Fatalf("filtered: %v", ids(even))
	}
	if got := j.Query(10, 5, nil); len(got) != 0 {
		t.Fatalf("query past end: %v", ids(got))
	}
	r, ok := j.LastBefore(t0.Add(4500*time.Millisecond), nil)
	if !ok || r.ID != 4 {
		t.Fatalf("LastBefore = %d %v", r.ID, ok)
	}
	if _, ok := j.LastBefore(t0, nil); ok {
		t.Fatal("LastBefore found a record before the first one")
	}
}

func ids(rs []Record) []uint64 {
	var out []uint64
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}
