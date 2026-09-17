package speedtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
func TestUploadStopsOnContextCancellation(t *testing.T) {
	server, _ := newCountingServer(t, func() { time.Sleep(10 * time.Millisecond) })
	// A capture window far longer than the test's patience: if cancellation is
	// ignored, this call blocks for the whole window.
	target := newTestTarget(t, server.URL, 30*time.Second, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := target.UploadTestContext(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline to surface, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cancellation took %v; the phase ran on after the context ended", elapsed)
	}
}

func TestDownloadStopsOnContextCancellation(t *testing.T) {
	server, _ := newCountingServer(t, func() { time.Sleep(10 * time.Millisecond) })
	target := newTestTarget(t, server.URL, 30*time.Second, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := target.DownloadTestContext(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline to surface, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cancellation took %v; the phase ran on after the context ended", elapsed)
	}
}

// An unreachable endpoint must be reported as such. Reporting nil folds a total
// failure into a rate of zero, which a caller cannot tell apart from an idle
// link.
func TestUploadReportsUnreachableServer(t *testing.T) {
	server, _ := newCountingServer(t, nil)
	url := server.URL
	server.Close()

	target := newTestTarget(t, url, 300*time.Millisecond, 1)

	if err := target.UploadTestContext(context.Background()); !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("expected ErrConnectTimeout, got %v", err)
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
