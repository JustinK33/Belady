package obs

import (
	"log/slog"
	"runtime"
	"testing"
)

// Contention profiling must default to off. It costs throughput on every lock in
// the process, so a service that switched it on by accident would quietly report
// worse numbers than it is capable of.
func TestContentionProfilingDefaultsOff(t *testing.T) {
	enableContentionProfiles(slog.Default())

	// A negative argument reads the current setting without changing it.
	if got := runtime.SetMutexProfileFraction(-1); got != 0 {
		t.Errorf("mutex profile fraction = %d, want 0", got)
	}
}

func TestContentionProfilingEnabled(t *testing.T) {
	t.Setenv("PROFILE_CONTENTION", "true")
	// Leaving the samplers armed would slow down every test that follows in this
	// binary, which is exactly the cost the default protects against.
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	})

	enableContentionProfiles(slog.Default())

	if got := runtime.SetMutexProfileFraction(-1); got != 1 {
		t.Errorf("mutex profile fraction = %d, want 1", got)
	}
}
