package market

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"unicode"
)

// Key identifies a market within a match.
type Key struct {
	MatchID  string `json:"matchId"`
	MarketID string `json:"marketId"`
}

func (k Key) String() string { return k.MatchID + "/" + k.MarketID }

// Outcome identifies one selectable result of a market, e.g. "falcons".
type Outcome string

// Status is the availability of a market, independent of its prices.
type Status string

const (
	// StatusUnknown is reported until the feed has told us the status.
	// It is never accepted from the feed.
	StatusUnknown   Status = "unknown"
	StatusOpen      Status = "open"
	StatusSuspended Status = "suspended"
	StatusClosed    Status = "closed"
)

// ParseStatus accepts only the statuses a feed may send.
func ParseStatus(s string) (Status, error) {
	switch Status(s) {
	case StatusOpen, StatusSuspended, StatusClosed:
		return Status(s), nil
	}
	return "", fmt.Errorf("unknown status %q", s)
}

// MaxSeq keeps sequence numbers exactly representable in JavaScript
// (Number.MAX_SAFE_INTEGER), since clients receive them as JSON numbers.
const MaxSeq = 1<<53 - 1

// Event is one delta from the feed. Fields that are absent leave the
// corresponding part of the market untouched: prices and availability
// change independently.
type Event struct {
	Key    Key
	Seq    uint64
	Status Status            // "" when the event does not change availability
	Odds   map[Outcome]Price // only the outcomes whose price is (re)stated
}

// Snapshot is the complete state of a market as of Seq, used to resync
// after an interruption. Unlike an Event every field is mandatory.
type Snapshot struct {
	Key    Key
	Seq    uint64
	Status Status
	Odds   map[Outcome]Price
}

// ValidationError marks input that was rejected as malformed.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &ValidationError{fmt.Sprintf(format, args...)}
}

// IsValidation reports whether err is a rejection of malformed input.
func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

// wireMessage captures every member raw so that absent, null and
// wrongly-typed values can be told apart and rejected explicitly.
type wireMessage struct {
	MatchID  jsontext.Value `json:"matchId"`
	MarketID jsontext.Value `json:"marketId"`
	Seq      jsontext.Value `json:"seq"`
	Status   jsontext.Value `json:"status"`
	Odds     jsontext.Value `json:"odds"`
}

// decodeWire parses one feed object. Parsing is deliberately strict:
// member names are case-sensitive, duplicate names and unknown members are
// rejected. A silently ignored "Status" or a second "seq" could otherwise
// make the board show something the feed never meant.
func decodeWire(data []byte) (wireMessage, error) {
	var w wireMessage
	if k := jsontext.Value(bytes.TrimSpace(data)).Kind(); k != '{' {
		return w, invalid("message must be a JSON object")
	}
	if err := json.Unmarshal(data, &w, json.RejectUnknownMembers(true)); err != nil {
		return w, invalid("malformed message: %v", err)
	}
	return w, nil
}

// ParseEvent parses and validates a single feed event.
func ParseEvent(data []byte) (Event, error) {
	w, err := decodeWire(data)
	if err != nil {
		return Event{}, err
	}
	key, err := w.key()
	if err != nil {
		return Event{}, err
	}
	seq, err := parseSeq(w.Seq)
	if err != nil {
		return Event{}, err
	}
	ev := Event{Key: key, Seq: seq}
	if w.Status != nil {
		if ev.Status, err = parseStatusValue(w.Status); err != nil {
			return Event{}, err
		}
	}
	if w.Odds != nil {
		if ev.Odds, err = parseOdds(w.Odds); err != nil {
			return Event{}, err
		}
	}
	return ev, nil
}

// ParseSnapshot parses and validates a full market snapshot. Whether the
// odds cover exactly the market's outcomes is checked by the Market.
func ParseSnapshot(data []byte) (Snapshot, error) {
	w, err := decodeWire(data)
	if err != nil {
		return Snapshot{}, err
	}
	key, err := w.key()
	if err != nil {
		return Snapshot{}, err
	}
	seq, err := parseSeq(w.Seq)
	if err != nil {
		return Snapshot{}, err
	}
	if w.Status == nil {
		return Snapshot{}, invalid("snapshot: status is required")
	}
	status, err := parseStatusValue(w.Status)
	if err != nil {
		return Snapshot{}, err
	}
	if w.Odds == nil {
		return Snapshot{}, invalid("snapshot: odds are required")
	}
	odds, err := parseOdds(w.Odds)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Key: key, Seq: seq, Status: status, Odds: odds}, nil
}

// Peek extracts the market key and seq from a message on a best-effort
// basis, so that even rejected messages can be filed under their market in
// the audit journal. It never fails; unknown parts are left zero.
func Peek(data []byte) (Key, uint64) {
	var w struct {
		MatchID  string         `json:"matchId"`
		MarketID string         `json:"marketId"`
		Seq      jsontext.Value `json:"seq"`
	}
	_ = json.Unmarshal(data, &w, jsontext.AllowDuplicateNames(true))
	seq, _ := strconv.ParseUint(string(bytes.TrimSpace(w.Seq)), 10, 64)
	return Key{MatchID: w.MatchID, MarketID: w.MarketID}, seq
}

func (w wireMessage) key() (Key, error) {
	match, err := parseID("matchId", w.MatchID)
	if err != nil {
		return Key{}, err
	}
	mkt, err := parseID("marketId", w.MarketID)
	if err != nil {
		return Key{}, err
	}
	return Key{MatchID: match, MarketID: mkt}, nil
}

func parseID(name string, v jsontext.Value) (string, error) {
	if v == nil {
		return "", invalid("%s is required", name)
	}
	if v.Kind() != '"' {
		return "", invalid("%s must be a string", name)
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", invalid("%s: %v", name, err)
	}
	if s == "" || len(s) > 128 {
		return "", invalid("%s must be 1-128 characters", name)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return "", invalid("%s contains an invalid character", name)
		}
	}
	return s, nil
}

func parseSeq(v jsontext.Value) (uint64, error) {
	if v == nil {
		return 0, invalid("seq is required")
	}
	if v.Kind() != '0' {
		return 0, invalid("seq must be a number")
	}
	// ParseUint rejects fractions, exponents and signs: "41.0" or "4.1e1"
	// are not accepted as 41.
	seq, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 10, 64)
	if err != nil || seq == 0 || seq > MaxSeq {
		return 0, invalid("seq must be an integer between 1 and %d", uint64(MaxSeq))
	}
	return seq, nil
}

func parseStatusValue(v jsontext.Value) (Status, error) {
	if v.Kind() != '"' {
		return "", invalid("status must be a string")
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", invalid("status: %v", err)
	}
	st, err := ParseStatus(s)
	if err != nil {
		return "", invalid("%v", err)
	}
	return st, nil
}

func parseOdds(v jsontext.Value) (map[Outcome]Price, error) {
	if v.Kind() != '{' {
		return nil, invalid("odds must be an object")
	}
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(v, &raw); err != nil {
		return nil, invalid("odds: %v", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	odds := make(map[Outcome]Price, len(raw))
	for name, val := range raw {
		if val.Kind() != '0' {
			return nil, invalid("odds.%s must be a number", name)
		}
		p, err := ParsePrice(string(bytes.TrimSpace(val)))
		if err != nil {
			return nil, invalid("odds.%s: %v", name, err)
		}
		odds[Outcome(name)] = p
	}
	return odds, nil
}

// EncodeEvent renders ev in the feed's wire format.
func EncodeEvent(ev Event) []byte {
	return encodeWire(ev.Key, ev.Seq, ev.Status, ev.Odds)
}

// EncodeSnapshot renders s in the feed's wire format.
func EncodeSnapshot(s Snapshot) []byte {
	return encodeWire(s.Key, s.Seq, s.Status, s.Odds)
}

func encodeWire(k Key, seq uint64, st Status, odds map[Outcome]Price) []byte {
	var b bytes.Buffer
	id := func(s string) string { out, _ := json.Marshal(s); return string(out) }
	fmt.Fprintf(&b, `{"matchId":%s,"marketId":%s,"seq":%d`, id(k.MatchID), id(k.MarketID), seq)
	if st != "" {
		fmt.Fprintf(&b, `,"status":%s`, id(string(st)))
	}
	if len(odds) > 0 {
		b.WriteString(`,"odds":{`)
		for i, o := range slices.Sorted(maps.Keys(odds)) {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `%s:%s`, id(string(o)), odds[o])
		}
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return b.Bytes()
}

// SplitBatch accepts either a single JSON object or a JSON array of values
// and returns each item's raw bytes, so every item can be processed and
// journaled on its own.
func SplitBatch(data []byte) ([]jsontext.Value, error) {
	trimmed := bytes.TrimSpace(data)
	switch jsontext.Value(trimmed).Kind() {
	case '{':
		if !jsontext.Value(trimmed).IsValid() {
			return nil, invalid("body is not valid JSON")
		}
		return []jsontext.Value{jsontext.Value(trimmed)}, nil
	case '[':
		var items []jsontext.Value
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, invalid("body is not valid JSON: %v", err)
		}
		return items, nil
	}
	return nil, invalid("body must be a JSON object or an array of objects")
}
