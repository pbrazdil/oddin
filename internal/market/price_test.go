package market

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestParsePriceAccepts(t *testing.T) {
	tests := []struct {
		in   string
		want Price
	}{
		{"1.85", 18500},
		{"2.05", 20500},
		{"1.8", 18000},
		{"1.80", 18000},
		{"1.8000", 18000},
		{"1.85000000", 18500}, // trailing zeros carry no precision
		{"2", 20000},
		{"1.0001", 10001},
		{"1.333", 13330},
		{"1000", 10_000_000},
		{"1000.0000", 10_000_000},
		{"185e-2", 18500},
		{"1.85E0", 18500},
		{"0.185e1", 18500},
		{"1.85e+0", 18500},
		{"18500e-4", 18500},
	}
	for _, tt := range tests {
		got, err := ParsePrice(tt.in)
		if err != nil {
			t.Errorf("ParsePrice(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParsePrice(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParsePriceRejects(t *testing.T) {
	tests := []struct {
		in, why string
	}{
		{"1", "even odds pay nothing"},
		{"1.0", "even odds pay nothing"},
		{"1.00001", "five decimals would need rounding"},
		{"1.85000001", "precision would be lost"},
		{"0.5", "below one"},
		{"0", "zero"},
		{"-0", "zero"},
		{"-1.85", "negative"},
		{"1000.0001", "above max"},
		{"1e4", "above max"},
		{"1e99999", "huge exponent"},
		{"1e-99999", "tiny exponent"},
		{"99999999999999999999", "overflow"},
		{"", "empty"},
		{"abc", "not a number"},
		{"1.", "dangling dot"},
		{".5", "missing integer part"},
		{"01.5", "leading zero"},
		{"1.5.5", "two dots"},
		{"+1.5", "plus sign is not JSON"},
		{"1e", "dangling exponent"},
		{"1e+", "dangling exponent sign"},
		{"1.85 ", "trailing space"},
		{"NaN", "not JSON"},
		{"Infinity", "not JSON"},
		{"0x10", "hex"},
		{"1_000", "underscore"},
		{"3/2", "fraction"},
	}
	for _, tt := range tests {
		if got, err := ParsePrice(tt.in); err == nil {
			t.Errorf("ParsePrice(%q) = %v, want error (%s)", tt.in, got, tt.why)
		}
	}
}

func TestPriceString(t *testing.T) {
	tests := []struct {
		in   Price
		want string
	}{
		{18500, "1.85"},
		{18000, "1.80"},
		{10001, "1.0001"},
		{13330, "1.333"},
		{20000, "2.00"},
		{10_000_000, "1000.00"},
		{0, "0.00"},
		{-18500, "-1.85"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Price(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPriceJSONRoundTrip(t *testing.T) {
	p := MustPrice("1.85")
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1.85"` {
		t.Fatalf("marshal = %s, want a JSON string", b)
	}
	var back Price
	if err := json.Unmarshal(b, &back); err != nil || back != p {
		t.Fatalf("unmarshal string = %v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`2.05`), &back); err != nil || back != 20500 {
		t.Fatalf("unmarshal number = %v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`"0.5"`), &back); err == nil {
		t.Fatal("unmarshal accepted an invalid price")
	}
}

func FuzzParsePrice(f *testing.F) {
	for _, s := range []string{"1.85", "2", "1.0001", "185e-2", "1e4", "-1", "1.00001", "1e-9999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParsePrice(s)
		if err != nil {
			return
		}
		if p < MinPrice || p > MaxPrice {
			t.Fatalf("ParsePrice(%q) = %d outside bounds", s, p)
		}
		// Formatting must be lossless.
		back, err := ParsePrice(p.String())
		if err != nil || back != p {
			t.Fatalf("round trip of %q via %q = %d, %v", s, p.String(), back, err)
		}
		if strings.ContainsAny(p.String(), "eE") {
			t.Fatalf("String() of %q uses an exponent: %s", s, p.String())
		}
	})
}
