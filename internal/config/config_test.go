package config

import "testing"

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"5GB", 5_000_000_000, false},
		{"5gib", 5 << 30, false},
		{"512MiB", 512 << 20, false},
		{"1024", 1024, false},
		{"  10 mb ", 10_000_000, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
		{"1XB", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseSize(%q) expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
