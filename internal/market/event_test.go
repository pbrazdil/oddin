package market

import (
	"maps"
	"strings"
	"testing"
)

func TestParseEventSpecExample(t *testing.T) {
	tests := []struct {
		in   string
		want Event
	}{
		{
			`{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`,
			Event{Key: testKey, Seq: 41, Status: StatusOpen, Odds: map[Outcome]Price{"falcons": 18500, "wolves": 20500}},
		},
		{
			`{"matchId":"match-1","marketId":"winner","seq":42,"odds":{"falcons":1.80}}`,
			Event{Key: testKey, Seq: 42, Odds: map[Outcome]Price{"falcons": 18000}},
		},
		{
			`{"matchId":"match-1","marketId":"winner","seq":43,"status":"suspended"}`,
			Event{Key: testKey, Seq: 43, Status: StatusSuspended},
		},
		{
			`{"matchId":"match-1","marketId":"winner","seq":44,"status":"open"}`,
			Event{Key: testKey, Seq: 44, Status: StatusOpen},
		},
		{
			`{"matchId":"match-1","marketId":"winner","seq":45,"odds":{"falcons":1.90,"wolves":1.95}}`,
			Event{Key: testKey, Seq: 45, Odds: map[Outcome]Price{"falcons": 19000, "wolves": 19500}},
		},
		{
			// Whitespace and member order are irrelevant.
			"  {\n \"seq\": 7, \"odds\": {}, \"marketId\": \"winner\", \"matchId\": \"match-1\" }\n",
			Event{Key: testKey, Seq: 7},
		},
		{
			`{"matchId":"match-1","marketId":"winner","seq":9,"status":"closed"}`,
			Event{Key: testKey, Seq: 9, Status: StatusClosed},
		},
	}
	for _, tt := range tests {
		got, err := ParseEvent([]byte(tt.in))
		if err != nil {
			t.Errorf("ParseEvent(%s) error: %v", tt.in, err)
			continue
		}
		if got.Key != tt.want.Key || got.Seq != tt.want.Seq || got.Status != tt.want.Status || !maps.Equal(got.Odds, tt.want.Odds) {
			t.Errorf("ParseEvent(%s) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseEventRejects(t *testing.T) {
	base := `"matchId":"match-1","marketId":"winner"`
	tests := []struct {
		name, in, wantErr string
	}{
		{"not json", `{`, "malformed"},
		{"array", `[]`, "must be a JSON object"},
		{"string", `"hi"`, "must be a JSON object"},
		{"null", `null`, "must be a JSON object"},
		{"trailing data", `{` + base + `,"seq":1}{}`, "malformed"},
		{"missing seq", `{` + base + `}`, "seq is required"},
		{"seq zero", `{` + base + `,"seq":0}`, "seq must be an integer"},
		{"seq negative", `{` + base + `,"seq":-1}`, "seq must be an integer"},
		{"seq fraction", `{` + base + `,"seq":41.5}`, "seq must be an integer"},
		{"seq with decimal point", `{` + base + `,"seq":41.0}`, "seq must be an integer"},
		{"seq exponent", `{` + base + `,"seq":4.1e1}`, "seq must be an integer"},
		{"seq string", `{` + base + `,"seq":"41"}`, "seq must be a number"},
		{"seq null", `{` + base + `,"seq":null}`, "seq must be a number"},
		{"seq beyond js safe integer", `{` + base + `,"seq":9007199254740992}`, "seq must be an integer"},
		{"seq overflow", `{` + base + `,"seq":99999999999999999999999}`, "seq must be an integer"},
		{"missing matchId", `{"marketId":"winner","seq":1}`, "matchId is required"},
		{"missing marketId", `{"matchId":"match-1","seq":1}`, "marketId is required"},
		{"empty matchId", `{"matchId":"","marketId":"winner","seq":1}`, "matchId must be"},
		{"numeric matchId", `{"matchId":1,"marketId":"winner","seq":1}`, "matchId must be a string"},
		{"matchId with space", `{"matchId":"match 1","marketId":"winner","seq":1}`, "invalid character"},
		{"status null", `{` + base + `,"seq":1,"status":null}`, "status must be a string"},
		{"status number", `{` + base + `,"seq":1,"status":1}`, "status must be a string"},
		{"status unknown", `{` + base + `,"seq":1,"status":"paused"}`, "unknown status"},
		{"status wrong case", `{` + base + `,"seq":1,"status":"Open"}`, "unknown status"},
		{"status unknown sentinel", `{` + base + `,"seq":1,"status":"unknown"}`, "unknown status"},
		{"odds null", `{` + base + `,"seq":1,"odds":null}`, "odds must be an object"},
		{"odds array", `{` + base + `,"seq":1,"odds":[1.8]}`, "odds must be an object"},
		{"odds string value", `{` + base + `,"seq":1,"odds":{"falcons":"1.85"}}`, "must be a number"},
		{"odds null value", `{` + base + `,"seq":1,"odds":{"falcons":null}}`, "must be a number"},
		{"odds too low", `{` + base + `,"seq":1,"odds":{"falcons":1.0}}`, "greater than 1.00"},
		{"odds negative", `{` + base + `,"seq":1,"odds":{"falcons":-2}}`, "greater than 1.00"},
		{"odds too precise", `{` + base + `,"seq":1,"odds":{"falcons":1.85001}}`, "decimal places"},
		{"odds too high", `{` + base + `,"seq":1,"odds":{"falcons":1000.01}}`, "at most"},
		{"duplicate odds key", `{` + base + `,"seq":1,"odds":{"falcons":1.8,"falcons":1.9}}`, "duplicate"},
		{"duplicate seq key", `{` + base + `,"seq":1,"seq":2}`, "duplicate"},
		{"duplicate status key", `{` + base + `,"seq":1,"status":"open","status":"suspended"}`, "duplicate"},
		{"unknown member", `{` + base + `,"seq":1,"price":1.8}`, "malformed"},
		{"wrong-case member", `{` + base + `,"seq":1,"Status":"suspended"}`, "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseEvent([]byte(tt.in))
			if err == nil {
				t.Fatalf("ParseEvent(%s) succeeded, want error containing %q", tt.in, tt.wantErr)
			}
			if !IsValidation(err) {
				t.Errorf("error %v is not a validation error", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseSnapshot(t *testing.T) {
	s, err := ParseSnapshot([]byte(`{"matchId":"match-1","marketId":"winner","seq":45,"status":"open","odds":{"falcons":1.90,"wolves":1.95}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Seq != 45 || s.Status != StatusOpen || s.Odds["falcons"] != 19000 || s.Odds["wolves"] != 19500 {
		t.Fatalf("unexpected snapshot %+v", s)
	}
	for _, in := range []string{
		`{"matchId":"match-1","marketId":"winner","seq":45,"odds":{"falcons":1.90,"wolves":1.95}}`,
		`{"matchId":"match-1","marketId":"winner","seq":45,"status":"open"}`,
		`{"matchId":"match-1","marketId":"winner","status":"open","odds":{"falcons":1.90,"wolves":1.95}}`,
	} {
		if _, err := ParseSnapshot([]byte(in)); err == nil {
			t.Errorf("ParseSnapshot(%s) accepted an incomplete snapshot", in)
		}
	}
}

func TestEncodeEventRoundTrip(t *testing.T) {
	events := []Event{
		{Key: testKey, Seq: 41, Status: StatusOpen, Odds: map[Outcome]Price{"falcons": 18500, "wolves": 20500}},
		{Key: testKey, Seq: 42, Odds: map[Outcome]Price{"falcons": 18000}},
		{Key: testKey, Seq: 43, Status: StatusSuspended},
		{Key: testKey, Seq: 44},
		{Key: Key{"m\"x", "w\\y"}, Seq: 1, Status: StatusClosed},
	}
	for _, ev := range events {
		raw := EncodeEvent(ev)
		got, err := ParseEvent(raw)
		if err != nil {
			t.Fatalf("ParseEvent(%s): %v", raw, err)
		}
		if got.Key != ev.Key || got.Seq != ev.Seq || got.Status != ev.Status || !maps.Equal(got.Odds, ev.Odds) {
			t.Errorf("round trip of %+v gave %+v", ev, got)
		}
	}
}

func TestSplitBatch(t *testing.T) {
	items, err := SplitBatch([]byte(`[{"a":1}, {"b":2}, 3]`))
	if err != nil || len(items) != 3 || string(items[1]) != `{"b":2}` {
		t.Fatalf("array: %q, %v", items, err)
	}
	items, err = SplitBatch([]byte(` {"a":1} `))
	if err != nil || len(items) != 1 || string(items[0]) != `{"a":1}` {
		t.Fatalf("object: %q, %v", items, err)
	}
	for _, in := range []string{``, `nope`, `"x"`, `{`, `[{"a":1}`, `[1] [2]`} {
		if _, err := SplitBatch([]byte(in)); err == nil {
			t.Errorf("SplitBatch(%q) succeeded", in)
		}
	}
}

func TestPeek(t *testing.T) {
	k, seq := Peek([]byte(`{"matchId":"match-1","marketId":"winner","seq":43,"status":"bogus"}`))
	if k != testKey || seq != 43 {
		t.Fatalf("Peek = %v %d", k, seq)
	}
	k, seq = Peek([]byte(`garbage`))
	if k != (Key{}) || seq != 0 {
		t.Fatalf("Peek(garbage) = %v %d", k, seq)
	}
}

func FuzzParseEvent(f *testing.F) {
	for _, s := range []string{
		`{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`,
		`{"matchId":"match-1","marketId":"winner","seq":42,"odds":{"falcons":1.80}}`,
		`{"matchId":"match-1","marketId":"winner","seq":43,"status":"suspended"}`,
		`{"matchId":"m","marketId":"w","seq":1,"odds":{"a":1e1}}`,
		`[]`, `{}`, `null`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := ParseEvent(data)
		if err != nil {
			if !IsValidation(err) {
				t.Fatalf("non-validation error: %v", err)
			}
			return
		}
		if ev.Seq == 0 || ev.Seq > MaxSeq {
			t.Fatalf("accepted seq %d", ev.Seq)
		}
		for o, p := range ev.Odds {
			if p < MinPrice || p > MaxPrice {
				t.Fatalf("accepted odds.%s = %d", o, p)
			}
		}
		// Whatever we accept must survive a canonical re-encoding unchanged.
		again, err := ParseEvent(EncodeEvent(ev))
		if err != nil {
			t.Fatalf("re-parse of %s: %v", EncodeEvent(ev), err)
		}
		if again.Key != ev.Key || again.Seq != ev.Seq || again.Status != ev.Status || !maps.Equal(again.Odds, ev.Odds) {
			t.Fatalf("re-parse mismatch: %+v vs %+v", ev, again)
		}
	})
}
