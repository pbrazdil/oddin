package market

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Price is decimal odds held as an exact fixed-point integer in units of
// 1/PriceScale. Odds are never stored as binary floating point: a feed value
// of 1.85 stays exactly 1.85 from ingestion to the screen and the audit log.
type Price int64

const (
	PriceScale    = 10_000
	priceDecimals = 4

	// MinPrice is the smallest accepted decimal odds. Odds of 1.00 or less
	// pay out no more than the stake and can only come from a broken feed.
	MinPrice Price = PriceScale + 1
	// MaxPrice rejects fat-finger or corrupted prices.
	MaxPrice Price = 1000 * PriceScale
)

var (
	errNumberSyntax = errors.New("not a JSON number")
	errTooPrecise   = fmt.Errorf("more than %d decimal places", priceDecimals)
	errOutOfRange   = errors.New("out of range")
)

// ParsePrice parses a JSON number literal into a Price. It never rounds:
// values that are not exactly representable with four decimal places, or
// that fall outside (1.00, 1000.00], are rejected.
func ParsePrice(lit string) (Price, error) {
	units, err := parseDecimal(lit, priceDecimals)
	if err != nil {
		return 0, fmt.Errorf("invalid odds %s: %w", lit, err)
	}
	p := Price(units)
	if p < MinPrice || p > MaxPrice {
		return 0, fmt.Errorf("invalid odds %s: must be greater than 1.00 and at most %s", lit, MaxPrice)
	}
	return p, nil
}

// MustPrice parses a price literal and panics on error. For tests and
// static configuration only.
func MustPrice(lit string) Price {
	p, err := ParsePrice(lit)
	if err != nil {
		panic(err)
	}
	return p
}

// parseDecimal converts a JSON number literal to an integer scaled by
// 10^decimals, failing instead of rounding when precision would be lost.
//
// JSON grammar: -? (0 | [1-9][0-9]*) (\.[0-9]+)? ([eE][+-]?[0-9]+)?
func parseDecimal(lit string, decimals int) (int64, error) {
	s := lit
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}

	n := leadingDigits(s)
	intPart := s[:n]
	if intPart == "" || (len(intPart) > 1 && intPart[0] == '0') {
		return 0, errNumberSyntax
	}
	s = s[n:]

	frac := ""
	if strings.HasPrefix(s, ".") {
		s = s[1:]
		n = leadingDigits(s)
		if n == 0 {
			return 0, errNumberSyntax
		}
		frac, s = s[:n], s[n:]
	}

	exp := 0
	if s != "" && (s[0] == 'e' || s[0] == 'E') {
		s = s[1:]
		sign := 1
		if s != "" && (s[0] == '+' || s[0] == '-') {
			if s[0] == '-' {
				sign = -1
			}
			s = s[1:]
		}
		n = leadingDigits(s)
		if n == 0 || n != len(s) {
			return 0, errNumberSyntax
		}
		// Bound the exponent before converting so "1e999999999" cannot
		// trigger huge allocations; no sane price needs |exp| > 9999.
		if n > 4 {
			return 0, errOutOfRange
		}
		e, _ := strconv.Atoi(s)
		exp, s = sign*e, ""
	}
	if s != "" {
		return 0, errNumberSyntax
	}

	// value = digits × 10^(exp − len(frac)); we need value × 10^decimals.
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return 0, nil
	}
	shift := exp - len(frac) + decimals
	if shift < 0 {
		keep := len(digits) + shift
		if keep <= 0 || strings.Trim(digits[keep:], "0") != "" {
			return 0, errTooPrecise
		}
		digits = digits[:keep]
	} else {
		if len(digits)+shift > 18 {
			return 0, errOutOfRange
		}
		digits += strings.Repeat("0", shift)
	}
	if len(digits) > 18 {
		return 0, errOutOfRange
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, errOutOfRange
	}
	if neg {
		v = -v
	}
	return v, nil
}

func leadingDigits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// String formats the price with at least two and at most four decimals,
// e.g. "1.80", "2.05", "1.333".
func (p Price) String() string {
	sign := ""
	u := uint64(p)
	if p < 0 {
		sign = "-"
		u = uint64(-(p + 1)) + 1 // safe for math.MinInt64
	}
	frac := fmt.Sprintf("%04d", u%PriceScale)
	frac = strings.TrimRight(frac, "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return fmt.Sprintf("%s%d.%s", sign, u/PriceScale, frac)
}

// MarshalJSON encodes the price as a JSON string so that clients never
// round-trip odds through binary floating point.
func (p Price) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.String() + `"`), nil
}

// UnmarshalJSON accepts either a JSON string or a JSON number.
func (p *Price) UnmarshalJSON(b []byte) error {
	lit := string(b)
	if len(lit) >= 2 && lit[0] == '"' && lit[len(lit)-1] == '"' {
		lit = lit[1 : len(lit)-1]
	}
	v, err := ParsePrice(lit)
	if err != nil {
		return err
	}
	*p = v
	return nil
}
