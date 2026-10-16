// Package journal is the append-only audit trail of everything the board
// received and every state it published. It is the track record used to
// explain to a customer what the board showed at any moment, and the
// source the board rebuilds its state from after a restart.
//
// Each record is chained to its predecessor with SHA-256, so editing,
// removing or reordering lines in the file is detected on load.
package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"oddsboard/internal/market"
)

// Kind classifies a record.
type Kind string

const (
	KindEvent    Kind = "event"    // a feed delta, whatever its verdict
	KindSnapshot Kind = "snapshot" // a full-state resync
	KindHealth   Kind = "health"   // a state change caused by time alone (feed went stale)
	KindReset    Kind = "reset"    // a market was (re)initialised
	KindRecovery Kind = "recovery" // a failed attempt to fetch a snapshot
)

// Record is one journal entry.
type Record struct {
	ID       uint64          `json:"id"`
	At       time.Time       `json:"at"`
	Kind     Kind            `json:"kind"`
	Source   string          `json:"source,omitzero"`
	MatchID  string          `json:"matchId,omitzero"`
	MarketID string          `json:"marketId,omitzero"`
	Seq      uint64          `json:"seq,omitzero"`
	Payload  string          `json:"payload,omitzero"` // the message exactly as received
	Verdict  market.Verdict  `json:"verdict,omitzero"`
	Reason   string          `json:"reason,omitzero"`
	Changes  []market.Change `json:"changes,omitzero"`
	Summary  string          `json:"summary"`
	Epoch    string          `json:"epoch,omitzero"`
	Version  uint64          `json:"version,omitzero"`
	State    *market.State   `json:"state,omitzero"` // what the board showed after this record
}

// Key returns the market the record belongs to.
func (r Record) Key() market.Key { return market.Key{MatchID: r.MatchID, MarketID: r.MarketID} }

type entry struct {
	rec  Record
	raw  []byte // the record JSON the hash covers
	hash [32]byte
}

// Journal is safe for concurrent use.
type Journal struct {
	mu      sync.RWMutex
	path    string
	f       *os.File // nil for an in-memory journal
	entries []entry
	broken  error // set after a failed write; all further appends fail
}

// NewMemory returns a journal that keeps records in memory only.
func NewMemory() *Journal { return &Journal{} }

// Open loads and verifies the journal at path, creating it if needed, and
// opens it for appending. A torn final line (a write interrupted by a
// crash) is truncated: it was never acknowledged nor published, because
// records are synced to disk before the board commits them.
func Open(path string, log *slog.Logger) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	j := &Journal{path: path, f: f}
	good, err := j.load(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("journal %s: %w", path, err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	if good < size {
		log.Warn("truncating torn final journal record", "path", path, "bytes", size-good)
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(good, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return j, nil
}

// load reads and verifies every line, returning the offset just past the
// last good one.
func (j *Journal) load(r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var offset int64
	var prev [32]byte
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// Anything after the last newline is an append that never
			// completed. A complete but damaged line is an error instead:
			// silently dropping it could hide tampering.
			return offset, nil
		}
		if err != nil {
			return offset, err
		}
		e, err := decodeLine(line, prev)
		if err != nil {
			return offset, fmt.Errorf("line %d: %w", n, err)
		}
		if want := uint64(len(j.entries)) + 1; e.rec.ID != want {
			return offset, fmt.Errorf("line %d: record id %d, want %d", n, e.rec.ID, want)
		}
		j.entries = append(j.entries, e)
		prev = e.hash
		offset += int64(len(line))
	}
}

type envelope struct {
	Prev   string         `json:"prev"`
	Hash   string         `json:"hash"`
	Record jsontext.Value `json:"record"`
}

func decodeLine(line []byte, prev [32]byte) (entry, error) {
	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(line), &env); err != nil {
		return entry{}, fmt.Errorf("malformed record: %w", err)
	}
	if env.Prev != hex.EncodeToString(prev[:]) {
		return entry{}, errors.New("hash chain broken: record does not follow its predecessor")
	}
	h := chain(prev, env.Record)
	if env.Hash != hex.EncodeToString(h[:]) {
		return entry{}, errors.New("hash mismatch: record was modified")
	}
	var rec Record
	if err := json.Unmarshal(env.Record, &rec); err != nil {
		return entry{}, fmt.Errorf("malformed record body: %w", err)
	}
	return entry{rec: rec, raw: bytes.Clone(env.Record), hash: h}, nil
}

func chain(prev [32]byte, raw []byte) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	h.Write(raw)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Append assigns the next ID to r, writes it durably and returns the
// stored record. If it returns an error nothing was recorded and the
// caller must not act on r. After a write failure the journal refuses
// further appends, so a partially written line cannot be followed by more.
func (j *Journal) Append(r Record) (Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.broken != nil {
		return Record{}, fmt.Errorf("journal unavailable: %w", j.broken)
	}
	r.ID = uint64(len(j.entries)) + 1
	r.At = r.At.UTC()
	raw, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	var prev [32]byte
	if n := len(j.entries); n > 0 {
		prev = j.entries[n-1].hash
	}
	h := chain(prev, raw)
	if j.f != nil {
		line := make([]byte, 0, len(raw)+160)
		line = append(line, `{"prev":"`...)
		line = hex.AppendEncode(line, prev[:])
		line = append(line, `","hash":"`...)
		line = hex.AppendEncode(line, h[:])
		line = append(line, `","record":`...)
		line = append(line, raw...)
		line = append(line, "}\n"...)
		if _, err := j.f.Write(line); err != nil {
			j.broken = err
			return Record{}, err
		}
		if err := j.f.Sync(); err != nil {
			j.broken = err
			return Record{}, err
		}
	}
	// Store the record as it decodes from disk so in-memory and reloaded
	// journals are indistinguishable.
	var stored Record
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Record{}, err
	}
	j.entries = append(j.entries, entry{rec: stored, raw: raw, hash: h})
	return stored, nil
}

// Len returns the number of records.
func (j *Journal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.entries)
}

// Records returns all records in order.
func (j *Journal) Records() []Record {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]Record, len(j.entries))
	for i, e := range j.entries {
		out[i] = e.rec
	}
	return out
}

// Query returns up to limit records with ID > after that match keep.
func (j *Journal) Query(after uint64, limit int, keep func(Record) bool) []Record {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := []Record{}
	start := min(int(after), len(j.entries))
	for _, e := range j.entries[start:] {
		if keep != nil && !keep(e.rec) {
			continue
		}
		out = append(out, e.rec)
		if len(out) == limit {
			break
		}
	}
	return out
}

// LastBefore returns the latest record at or before t that matches keep.
func (j *Journal) LastBefore(t time.Time, keep func(Record) bool) (Record, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for i := len(j.entries) - 1; i >= 0; i-- {
		r := j.entries[i].rec
		if r.At.After(t) || (keep != nil && !keep(r)) {
			continue
		}
		return r, true
	}
	return Record{}, false
}

// Head returns the hash of the latest record, which commits to the entire
// history. Publishing it elsewhere makes later tampering detectable.
func (j *Journal) Head() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if len(j.entries) == 0 {
		return hex.EncodeToString(make([]byte, 32))
	}
	h := j.entries[len(j.entries)-1].hash
	return hex.EncodeToString(h[:])
}

// Verify re-checks the hash chain. For a file journal it re-reads the file,
// so it also detects modifications made on disk since startup.
func (j *Journal) Verify() (int, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.f == nil {
		var prev [32]byte
		for i, e := range j.entries {
			if chain(prev, e.raw) != e.hash {
				return i, fmt.Errorf("record %d: hash mismatch", e.rec.ID)
			}
			prev = e.hash
		}
		return len(j.entries), nil
	}
	f, err := os.Open(j.path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	check := &Journal{}
	if _, err := check.load(f); err != nil {
		return len(check.entries), err
	}
	if len(check.entries) != len(j.entries) {
		return len(check.entries), fmt.Errorf("file holds %d complete records, memory %d", len(check.entries), len(j.entries))
	}
	for i := range check.entries {
		if check.entries[i].hash != j.entries[i].hash {
			return i, fmt.Errorf("record %d differs from the file", i+1)
		}
	}
	return len(check.entries), nil
}

// Close closes the underlying file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	j.broken = errors.New("journal closed")
	return err
}
