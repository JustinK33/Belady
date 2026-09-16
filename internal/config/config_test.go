package config

import (
	"errors"
	"os"
	"os/exec"
	"strings"
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

func TestMustDuration(t *testing.T) {
	if got := MustDuration("BELADY_ABSENT_KEY", time.Second); got != time.Second {
		t.Errorf("MustDuration default = %v, want 1s", got)
	}

	t.Setenv("BELADY_TEST_MUST_DUR", "90s")
	if got := MustDuration("BELADY_TEST_MUST_DUR", time.Second); got != 90*time.Second {
		t.Errorf("MustDuration override = %v, want 90s", got)
	}
}

// TestMustDurationExitsOnAMalformedValue re-execs this test binary, because the
// whole point of MustDuration is os.Exit and there is no way to observe that from
// inside the process. The distinction from Duration's warn-and-fall-back is the
// only reason MustDuration exists, so it needs a test that actually sees the exit.
func TestMustDurationExitsOnAMalformedValue(t *testing.T) {
	if os.Getenv("BELADY_TEST_MUST_DUR_CHILD") == "1" {
		MustDuration("BELADY_TEST_MUST_DUR", time.Second)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMustDurationExitsOnAMalformedValue")
	cmd.Env = append(os.Environ(),
		"BELADY_TEST_MUST_DUR_CHILD=1",
		"BELADY_TEST_MUST_DUR=ten minutes",
	)
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child exited with %v, want a non-zero exit; output:\n%s", err, out)
	}
	if exit.ExitCode() != 2 {
		t.Errorf("child exit code = %d, want 2; output:\n%s", exit.ExitCode(), out)
	}
	if !strings.Contains(string(out), "BELADY_TEST_MUST_DUR") {
		t.Errorf("child stderr does not name the offending key:\n%s", out)
	}
}
