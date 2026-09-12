// SPDX-License-Identifier: Apache-2.0

package statutil

import "testing"

func TestMedian(t *testing.T) {
	cases := []struct {
		name string
		vals []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"odd", []float64{3, 1, 2}, 2},
		{"even", []float64{1, 2, 3, 4}, 2.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Median(c.vals); got != c.want {
				t.Errorf("Median(%v) = %v, want %v", c.vals, got, c.want)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	// Nearest-rank on these 10 values: rank(p) = round(p/100*9). p50 ->
	// round(4.5) = 5 -> sorted[5] = 60, not the interpolated 50 a
	// linear-interpolation percentile would give — this pins down which
	// convention every caller (computeEnergy's calibration block,
	// forge-compress's overhead percentiles) actually gets.
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("Percentile(nil, 50) = %v, want 0", got)
	}
	if got := Percentile(vals, 50); got != 60 {
		t.Errorf("Percentile(vals, 50) = %v, want 60 (nearest-rank)", got)
	}
	if got := Percentile(vals, 95); got != 100 {
		t.Errorf("Percentile(vals, 95) = %v, want 100 (nearest-rank)", got)
	}
	if got := Percentile(vals, 0); got != 10 {
		t.Errorf("Percentile(vals, 0) = %v, want 10 (the minimum)", got)
	}
	// Order independence.
	shuffled := []float64{100, 10, 90, 20, 80, 30, 70, 40, 60, 50}
	if got := Percentile(shuffled, 50); got != 60 {
		t.Errorf("Percentile(shuffled, 50) = %v, want 60", got)
	}
}
