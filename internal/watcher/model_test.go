package watcher

import "testing"

func TestNormalizePollInterval(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultPollIntervalSeconds},  // unset -> default
		{-5, defaultPollIntervalSeconds}, // negative -> default
		{1, minPollIntervalSeconds},      // below floor -> floor
		{9, minPollIntervalSeconds},      // just below floor -> floor
		{10, 10},                         // at floor -> unchanged
		{30, 30},                         // above floor -> unchanged
		{3600, 3600},                     // large -> unchanged
	}
	for _, c := range cases {
		if got := normalizePollInterval(c.in); got != c.want {
			t.Errorf("normalizePollInterval(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
