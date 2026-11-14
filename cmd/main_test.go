package main

import (
	"slices"
	"testing"
)

func TestJoinArgs(t *testing.T) {
	for _, tc := range []struct {
		in     []string
		rest   []string
		update bool
		bad    bool
	}{
		{in: nil},
		{in: []string{"-u"}, update: true},
		{in: []string{"--update"}, update: true},
		{in: []string{"1.2.3.4", "2302"}, rest: []string{"1.2.3.4", "2302"}},
		{in: []string{"1.2.3.4", "-u", "2302"}, rest: []string{"1.2.3.4", "2302"}, update: true},
		{in: []string{"-x"}, bad: true},
		{in: []string{"--updat"}, bad: true},
	} {
		rest, update, err := joinArgs(tc.in)
		if (err != nil) != tc.bad {
			t.Errorf("joinArgs(%q) err = %v, want error %v", tc.in, err, tc.bad)
			continue
		}
		if tc.bad {
			continue
		}
		if !slices.Equal(rest, tc.rest) || update != tc.update {
			t.Errorf("joinArgs(%q) = %q, %v; want %q, %v", tc.in, rest, update, tc.rest, tc.update)
		}
	}
}
