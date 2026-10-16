package market

// Range is an inclusive span of sequence numbers.
type Range struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// Ranges is a sorted set of disjoint, non-adjacent inclusive ranges.
// Methods never modify the receiver; they return a new set.
type Ranges []Range

// Add returns the set with r included, merging overlapping or adjacent spans.
func (rs Ranges) Add(r Range) Ranges {
	if r.From > r.To {
		return rs
	}
	out := make(Ranges, 0, len(rs)+1)
	placed := false
	for _, e := range rs {
		switch {
		case e.To < r.From && r.From-e.To > 1: // e lies strictly before r
			out = append(out, e)
		case r.To < e.From && e.From-r.To > 1: // e lies strictly after r
			if !placed {
				out = append(out, r)
				placed = true
			}
			out = append(out, e)
		default: // overlapping or adjacent: absorb e into r
			r.From = min(r.From, e.From)
			r.To = max(r.To, e.To)
		}
	}
	if !placed {
		out = append(out, r)
	}
	return out
}

// Remove returns the set with every number in r excluded.
func (rs Ranges) Remove(r Range) Ranges {
	if r.From > r.To {
		return rs
	}
	out := make(Ranges, 0, len(rs)+1)
	for _, e := range rs {
		if e.To < r.From || e.From > r.To {
			out = append(out, e)
			continue
		}
		if e.From < r.From {
			out = append(out, Range{e.From, r.From - 1})
		}
		if e.To > r.To {
			out = append(out, Range{r.To + 1, e.To})
		}
	}
	return out
}

// Contains reports whether x is in the set.
func (rs Ranges) Contains(x uint64) bool {
	for _, e := range rs {
		if x >= e.From && x <= e.To {
			return true
		}
	}
	return false
}

// AtLeast returns the part of the set that is >= x.
func (rs Ranges) AtLeast(x uint64) Ranges {
	out := Ranges{}
	for _, e := range rs {
		if e.To < x {
			continue
		}
		out = append(out, Range{max(e.From, x), e.To})
	}
	return out
}

// Count returns how many numbers the set holds.
func (rs Ranges) Count() uint64 {
	var n uint64
	for _, e := range rs {
		n += e.To - e.From + 1
	}
	return n
}
