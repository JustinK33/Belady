package obs

import (
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestStartServesHealthAndCancelsOnSignal covers the two promises four binaries now
// make through Start. Both failures are silent: a debug endpoint that never binds
// makes scripts/wait-for-health.sh time out on a service that is actually fine, and
// a context that ignores SIGTERM turns every graceful shutdown into a kill after
// Compose's grace period, which loses the recorder's last trace segment.
func TestStartServesHealthAndCancelsOnSignal(t *testing.T) {
	// Bind to find a free port, then release it. Racy in principle, but the
	// alternative is Serve returning its listener purely so a test can read a port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	t.Setenv("DEBUG_ADDR", addr)

	_, ctx, stop := Start("test")
	defer stop()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx // the deadline below is the timeout
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("debug endpoint never answered /healthz on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Signalling the test binary itself is safe only because Start registered a
	// handler: without one, SIGTERM would take the process down instead of failing.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled by SIGTERM")
	}
}

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
