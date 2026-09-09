package config

import (
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	ok := map[string]int64{
		"1024":  1024,
		"1b":    1,
		"1k":    1 << 10,
		"1kb":   1 << 10,
		"1KiB":  1 << 10,
		"512mb": 512 << 20,
		"4gb":   4 << 30,
		"1.5gb": 1<<30 + 512<<20,
		" 2 TB": 2 << 40,
	}
	for in, want := range ok {
		got, err := ParseBytes(in)
		if err != nil {
			t.Errorf("ParseBytes(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"", "mb", "-1gb", "twelve", "1zb"} {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) = %d, want error", in, got)
		}
	}
}

func TestDefaultsAndOverrides(t *testing.T) {
	if got := Int("BELADY_ABSENT_KEY", 7); got != 7 {
		t.Errorf("Int default = %d, want 7", got)
	}

	t.Setenv("BELADY_TEST_INT", "42")
	if got := Int("BELADY_TEST_INT", 7); got != 42 {
		t.Errorf("Int override = %d, want 42", got)
	}

	// A malformed value must fall back rather than crash the process.
	t.Setenv("BELADY_TEST_INT", "not-a-number")
	if got := Int("BELADY_TEST_INT", 7); got != 7 {
		t.Errorf("Int malformed = %d, want fallback 7", got)
	}

	t.Setenv("BELADY_TEST_DUR", "250ms")
	if got := Duration("BELADY_TEST_DUR", time.Second); got != 250*time.Millisecond {
		t.Errorf("Duration override = %v, want 250ms", got)
	}

	t.Setenv("BELADY_TEST_LIST", " a, ,b ,")
	got := Strings("BELADY_TEST_LIST", nil)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Strings = %q, want [a b]", got)
	}
}
