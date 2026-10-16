package market

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestRangesAdd(t *testing.T) {
	tests := []struct {
		name string
		in   Ranges
		add  Range
		want Ranges
	}{
		{"into empty", nil, Range{3, 5}, Ranges{{3, 5}}},
		{"before", Ranges{{10, 12}}, Range{3, 5}, Ranges{{3, 5}, {10, 12}}},
		{"after", Ranges{{3, 5}}, Range{10, 12}, Ranges{{3, 5}, {10, 12}}},
		{"adjacent below merges", Ranges{{6, 8}}, Range{3, 5}, Ranges{{3, 8}}},
		{"adjacent above merges", Ranges{{3, 5}}, Range{6, 8}, Ranges{{3, 8}}},
		{"overlap", Ranges{{3, 6}}, Range{5, 9}, Ranges{{3, 9}}},
		{"bridges two", Ranges{{1, 2}, {5, 6}}, Range{3, 4}, Ranges{{1, 6}}},
		{"swallows", Ranges{{4, 5}, {7, 8}}, Range{1, 10}, Ranges{{1, 10}}},
		{"inside", Ranges{{1, 10}}, Range{4, 5}, Ranges{{1, 10}}},
		{"empty range ignored", Ranges{{1, 2}}, Range{5, 4}, Ranges{{1, 2}}},
		{"single", Ranges{{1, 1}, {3, 3}}, Range{2, 2}, Ranges{{1, 3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.in)
			got := tt.in.Add(tt.add)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Add = %v, want %v", got, tt.want)
			}
			if !slices.Equal(tt.in, before) {
				t.Errorf("Add modified its receiver: %v", tt.in)
			}
		})
	}
}

func TestRangesRemove(t *testing.T) {
	tests := []struct {
		name string
		in   Ranges
		rm   Range
		want Ranges
	}{
		{"from empty", nil, Range{1, 5}, Ranges{}},
		{"exact", Ranges{{3, 5}}, Range{3, 5}, Ranges{}},
		{"split", Ranges{{1, 10}}, Range{4, 5}, Ranges{{1, 3}, {6, 10}}},
		{"trim low", Ranges{{1, 10}}, Range{1, 3}, Ranges{{4, 10}}},
		{"trim high", Ranges{{1, 10}}, Range{8, 12}, Ranges{{1, 7}}},
		{"across several", Ranges{{1, 2}, {4, 6}, {8, 9}}, Range{2, 8}, Ranges{{1, 1}, {9, 9}}},
		{"disjoint", Ranges{{1, 2}}, Range{5, 6}, Ranges{{1, 2}}},
		{"single point", Ranges{{42, 42}}, Range{42, 42}, Ranges{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Remove(tt.rm)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Remove = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRangesAtLeast(t *testing.T) {
	rs := Ranges{{1, 3}, {6, 9}, {12, 12}}
	tests := []struct {
		x    uint64
		want Ranges
	}{
		{0, rs},
		{2, Ranges{{2, 3}, {6, 9}, {12, 12}}},
		{4, Ranges{{6, 9}, {12, 12}}},
		{9, Ranges{{9, 9}, {12, 12}}},
		{12, Ranges{{12, 12}}},
		{13, Ranges{}},
	}
	for _, tt := range tests {
		if got := rs.AtLeast(tt.x); !slices.Equal(got, tt.want) {
			t.Errorf("AtLeast(%d) = %v, want %v", tt.x, got, tt.want)
		}
	}
}

// TestRangesAgainstBitmap checks random operation sequences against a
// trivially correct set model.
func TestRangesAgainstBitmap(t *testing.T) {
	const size = 64
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 2000 {
		var rs Ranges
		var model [size + 2]bool
		for range 20 {
			a := rng.Uint64N(size) + 1
			b := a + rng.Uint64N(8)
			if b > size {
				b = size
			}
			add := rng.IntN(2) == 0
			if add {
				rs = rs.Add(Range{a, b})
			} else {
				rs = rs.Remove(Range{a, b})
			}
			for x := a; x <= b; x++ {
				model[x] = add
			}
			// Invariant: sorted, disjoint, non-adjacent, non-empty.
			for i, e := range rs {
				if e.From > e.To || e.From == 0 {
					t.Fatalf("trial %d: bad range %v in %v", trial, e, rs)
				}
				if i > 0 && rs[i-1].To+1 >= e.From {
					t.Fatalf("trial %d: ranges not normalized: %v", trial, rs)
				}
			}
			var count uint64
			for x := uint64(1); x <= size; x++ {
				if rs.Contains(x) != model[x] {
					t.Fatalf("trial %d: Contains(%d) = %v, model %v; set %v", trial, x, rs.Contains(x), model[x], rs)
				}
				if model[x] {
					count++
				}
			}
			if rs.Count() != count {
				t.Fatalf("trial %d: Count = %d, want %d", trial, rs.Count(), count)
			}
		}
	}
}
