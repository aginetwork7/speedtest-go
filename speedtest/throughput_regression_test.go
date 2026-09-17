package speedtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newCountingServer records how many requests each client connection carried,
// keyed by remote address. A reused connection shows up as one address serving
// many requests.
func newCountingServer(t *testing.T, onRequest func()) (*httptest.Server, func() map[string]int) {
	t.Helper()

	var mu sync.Mutex
	perConn := map[string]int{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		perConn[r.RemoteAddr]++
		mu.Unlock()
		if onRequest != nil {
			onRequest()
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		snapshot := make(map[string]int, len(perConn))
		for addr, count := range perConn {
			snapshot[addr] = count
		}
		return snapshot
	}
}

func newTestTarget(t *testing.T, url string, capture time.Duration, threads int) *Server {
	t.Helper()

	client := New()
	client.SetCaptureTime(capture)
	client.SetNThread(threads)
	target, err := client.CustomServer(url)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	return target
}

// The upload body writer used to report io.EOF even after writing everything it
// was asked to. net/http reads that as a failed body, so it skips the
// terminating chunk and drops the connection, making every request pay a fresh
// handshake and restart congestion control.
func TestUploadReusesConnections(t *testing.T) {
	server, snapshot := newCountingServer(t, nil)
	target := newTestTarget(t, server.URL, 300*time.Millisecond, 1)

	if err := target.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("UploadTestContext: %v", err)
	}

	perConn := snapshot()
	total := 0
	for _, count := range perConn {
		total += count
	}
	if total < 2 {
		t.Fatalf("test did not issue enough requests to observe reuse: %d", total)
	}
	if len(perConn) != 1 {
		t.Fatalf("expected %d requests to share one connection, got %d connections", total, len(perConn))
	}
}

// A cancelled context has to stop the phase. The worker loop only watched an
// internal flag, so cancelling merely made every request fail instantly while
// the workers kept looping at full CPU until the capture timer expired.
//
// Both directions run the same contract off the same helper: kept apart, one
// copy of it would eventually stop asserting what the other does.
func TestPhaseStopsOnContextCancellation(t *testing.T) {
	phases := map[string]func(*Server) func(context.Context) error{
		"upload":   func(s *Server) func(context.Context) error { return s.UploadTestContext },
		"download": func(s *Server) func(context.Context) error { return s.DownloadTestContext },
	}

	for name, phase := range phases {
		t.Run(name, func(t *testing.T) {
			server, _ := newCountingServer(t, func() { time.Sleep(10 * time.Millisecond) })
			// A capture window far longer than the test's patience: if
			// cancellation is ignored, this call blocks for the whole window.
			target := newTestTarget(t, server.URL, 30*time.Second, 2)

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			start := time.Now()
			err := phase(target)(ctx)
			elapsed := time.Since(start)

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected the deadline to surface, got %v", err)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("cancellation took %v; the phase ran on after the context ended", elapsed)
			}
		})
	}
}

// An unreachable endpoint must be reported as such. Reporting nil folds a total
// failure into a rate of zero, which a caller cannot tell apart from an idle
// link.
//
// The multi-server entry points carry the same contract: they were the last
// place still folding a total failure into a successful-looking result.
func TestPhaseReportsUnreachableServer(t *testing.T) {
	phases := map[string]func(*Server) error{
		"upload":         func(s *Server) error { return s.UploadTestContext(context.Background()) },
		"download":       func(s *Server) error { return s.DownloadTestContext(context.Background()) },
		"multi upload":   func(s *Server) error { return s.MultiUploadTestContext(context.Background(), Servers{s}) },
		"multi download": func(s *Server) error { return s.MultiDownloadTestContext(context.Background(), Servers{s}) },
	}

	for name, phase := range phases {
		t.Run(name, func(t *testing.T) {
			server, _ := newCountingServer(t, nil)
			url := server.URL
			server.Close()

			target := newTestTarget(t, url, 300*time.Millisecond, 1)

			err := phase(target)
			if !errors.Is(err, ErrConnectTimeout) {
				t.Fatalf("expected ErrConnectTimeout, got %v", err)
			}
			// The sentinel says a phase failed; the cause says what to check.
			if err.Error() == ErrConnectTimeout.Error() {
				t.Fatal("the transport error behind the failure was dropped")
			}
		})
	}
}

// A cancelled multi-server run reports the cancellation, like the single-server
// one does.
func TestMultiPhaseReportsCancellation(t *testing.T) {
	server, _ := newCountingServer(t, func() { time.Sleep(10 * time.Millisecond) })
	target := newTestTarget(t, server.URL, 30*time.Second, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := target.MultiUploadTestContext(ctx, Servers{target}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline to surface, got %v", err)
	}
}

// The verdict must describe the phase that just ran. The byte counters live as
// long as the manager, so a phase reading them raw inherits the bytes of every
// phase before it and calls itself successful on them.
func TestPhaseVerdictCoversOnlyTheCurrentPhase(t *testing.T) {
	server, _ := newCountingServer(t, nil)
	target := newTestTarget(t, server.URL, 300*time.Millisecond, 1)

	if err := target.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("first phase: %v", err)
	}
	if target.Context.GetTotalUpload() == 0 {
		t.Fatal("the first phase transferred nothing, so the test proves nothing")
	}

	server.Close()

	if err := target.UploadTestContext(context.Background()); !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("a failed phase after a successful one must still fail, got %v", err)
	}
}

// Retrying a phase on the same server has to measure again. The handlers hold
// the context of the run that registered them, and that context is cancelled
// when the run ends, so handlers left installed can only fail.
func TestPhaseCanRunTwiceOnTheSameServer(t *testing.T) {
	server, _ := newCountingServer(t, nil)
	target := newTestTarget(t, server.URL, 300*time.Millisecond, 1)

	if err := target.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("first phase: %v", err)
	}
	afterFirst := target.Context.GetTotalUpload()

	if err := target.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("second phase on the same server: %v", err)
	}
	if target.Context.GetTotalUpload() <= afterFirst {
		t.Fatal("the second phase transferred nothing")
	}
	if target.ULSpeed <= 0 {
		t.Fatalf("the second phase reported no rate: %v", target.ULSpeed)
	}
}

// The upload meter outlives the phase that filled it, so a phase starting on a
// used direction has to clear it. Left in place, the previous phase's samples
// are averaged into this phase's rate.
func TestPhaseStartClearsTheUploadMeter(t *testing.T) {
	server, _ := newCountingServer(t, nil)
	target := newTestTarget(t, server.URL, 300*time.Millisecond, 1)

	if err := target.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("first phase: %v", err)
	}

	meter := target.Context.Manager.(*DataManager).upload.ackMeter
	// A sample no real phase could produce, standing in for whatever the
	// previous phase left behind.
	meter.ack(time.Now().Add(-100*time.Second), 1<<30)

	if err := target.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("second phase: %v", err)
	}

	meter.mu.Lock()
	seconds := meter.allSeconds
	meter.mu.Unlock()
	if seconds > 10 {
		t.Fatalf("the second phase measured %.0fs of transfer; it only ran for 0.3s, so it inherited the previous phase", seconds)
	}
}

// Every branch of the verdict, without a server in the way.
func TestPhaseErrorVerdicts(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	refused := errors.New("connection refused")
	allFailed := &phaseTally{}
	allFailed.note(refused)

	someSucceeded := &phaseTally{}
	someSucceeded.note(nil)
	someSucceeded.note(refused)

	cases := []struct {
		name        string
		ctx         context.Context
		tally       *phaseTally
		transferred int64
		want        error
		wantCause   bool
	}{
		{name: "cancelled", ctx: cancelled, tally: &phaseTally{}, want: context.Canceled},
		{name: "bytes moved", ctx: context.Background(), tally: allFailed, transferred: 1},
		{name: "no request dispatched", ctx: context.Background(), tally: &phaseTally{}, want: ErrConnectTimeout},
		{name: "every request failed", ctx: context.Background(), tally: allFailed, want: ErrConnectTimeout, wantCause: true},
		{name: "some request succeeded", ctx: context.Background(), tally: someSucceeded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := phaseError(tc.ctx, tc.tally, tc.transferred)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.wantCause && !strings.Contains(err.Error(), refused.Error()) {
				t.Fatalf("the cause was dropped: %v", err)
			}
		})
	}
}

// Constructing a client must not reroute unrelated traffic: the constructor
// installs itself as the transport of whatever client it holds.
func TestNewLeavesDefaultClientAlone(t *testing.T) {
	original := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = original })

	_ = New()

	if http.DefaultClient.Transport != original {
		t.Fatal("New replaced http.DefaultClient.Transport")
	}
}

// Workers exit when the context ends as well as when the capture window
// closes. On the context path nothing else stops the rate-capture goroutine, so
// it kept writing the Welford state while the caller read the final rate out of
// it. Run with -race.
func TestCancelledPhaseStopsTheRateCapture(t *testing.T) {
	server, _ := newCountingServer(t, func() { time.Sleep(5 * time.Millisecond) })
	target := newTestTarget(t, server.URL, 30*time.Second, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_ = target.DownloadTestContext(ctx)

	// Reading the rate after the phase returned must not race the capture
	// goroutine, so the phase has to have closed it down.
	_ = target.Context.GetEWMADownloadRate()
	_ = target.DLSpeed.Mbps()
}

// slowServer delivers a payload slowly enough that no request finishes inside
// the capture window, which is what a shaped or low-bandwidth link looks like.
func newSlowServer(t *testing.T, bytesPerTick int, tick time.Duration) *httptest.Server {
	t.Helper()
	chunk := make([]byte, bytesPerTick)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		flusher, _ := w.(http.Flusher)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(tick):
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Closing a phase cancels whatever is in flight. On a link too slow to finish
// one chunk inside the capture window every request ends cancelled, and
// counting those as failures reported a working link as unreachable — which
// excluded exactly the low-bandwidth links this client exists to measure.
func TestSlowLinkIsNotReportedAsUnreachable(t *testing.T) {
	server := newSlowServer(t, 16<<10, 20*time.Millisecond)
	target := newTestTarget(t, server.URL, 500*time.Millisecond, 1)

	if err := target.DownloadTestContext(context.Background()); err != nil {
		t.Fatalf("a link that delivered bytes must not be reported unreachable: %v", err)
	}
	if target.Context.GetTotalDownload() == 0 {
		t.Fatal("the test did not actually transfer anything")
	}
}
