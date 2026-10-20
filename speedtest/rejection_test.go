package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// refusingOrigin answers every request the way the deployment's authentication
// does when a credential is refused: an error status, sent without reading the
// request body, carrying a short text body of its own.
func refusingOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(origin.Close)
	return origin
}

// A refused download returns an error body, not the payload. Counting it would
// report the rejection's own bytes as throughput, and a phase made of nothing
// but rejections must say it was rejected rather than that the server could not
// be reached.
func TestDownloadReportsRejection(t *testing.T) {
	target := newTestTarget(t, refusingOrigin(t).URL, 500*time.Millisecond, 1)

	err := target.DownloadTestContext(context.Background())
	if !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("a download phase of refused requests returned %v, want ErrRequestRejected", err)
	}
	if errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("a refusal was reported as a connect timeout: %v", err)
	}
	if moved := target.Context.GetTotalDownload(); moved != 0 {
		t.Fatalf("the rejection's body was counted as %d downloaded bytes", moved)
	}
	if target.DLSpeed > 0 {
		t.Fatalf("a refused download reported %v of throughput", target.DLSpeed)
	}
}

// The latency probe times the round trip, which an error status makes just as
// a success does. The deployment serves no latency.txt and answers every probe
// 404, so a probe that demanded success failed every real run before it began.
func TestHTTPPingTimesAnyAnswer(t *testing.T) {
	target := newTestTarget(t, refusingOrigin(t).URL, time.Second, 1)

	latencies, err := target.HTTPPing(context.Background(), 3, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("a probe answered with an error status failed: %v", err)
	}
	if len(latencies) != 3 {
		t.Fatalf("got %d latency samples from 3 answered probes", len(latencies))
	}
}

// The error has to say what was refused and by whom, so the operator can tell a
// credential problem from a misrouted endpoint without a packet capture.
func TestRejectionNamesTheRequestAndStatus(t *testing.T) {
	origin := refusingOrigin(t)
	resp, err := http.Get(origin.URL + "/speedtest/random1000x1000.jpg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	got := checkStatus(resp, "download")
	if !errors.Is(got, ErrRequestRejected) {
		t.Fatalf("checkStatus(401) = %v, want ErrRequestRejected", got)
	}
	for _, want := range []string{"download", "/speedtest/random1000x1000.jpg", "401"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("rejection %q does not mention %q", got, want)
		}
	}
}

// Closing a phase cancels whatever is still in flight, so a cancellation is
// usually the last error a phase records. It must not replace the error that
// explains the phase. Whether a request happens to be in flight at the close is
// a matter of timing, so this is pinned on the tally itself rather than through
// a phase.
func TestPhaseTallyKeepsTheCauseOverCancellation(t *testing.T) {
	refused := fmt.Errorf("%w: download http://x/y: 401 Unauthorized", ErrRequestRejected)
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		tally := &phaseTally{}
		tally.note(refused)
		tally.note(fmt.Errorf("Get %q: %w", "http://x/y", cancellation))

		if cause := tally.cause(); !errors.Is(cause, ErrRequestRejected) {
			t.Errorf("after %v the phase cause is %v, want the rejection", cancellation, cause)
		}
	}

	// A cancellation is still kept when it is all there is.
	tally := &phaseTally{}
	tally.note(context.Canceled)
	if cause := tally.cause(); !errors.Is(cause, context.Canceled) {
		t.Errorf("a lone cancellation was dropped: cause is %v", cause)
	}
}

// Both upload entry points share the verdict; the multi-server one is checked
// separately so that it cannot drift back to reporting a refused phase as one
// that ran.
func TestMultiUploadReportsRejection(t *testing.T) {
	target := newTestTarget(t, refusingOrigin(t).URL, 500*time.Millisecond, 1)

	err := target.MultiUploadTestContext(context.Background(), Servers{target})
	if !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("a multi-server upload phase of refused requests returned %v, want ErrRequestRejected", err)
	}
}

// An upload's bytes are counted when they are handed to the socket, not when
// the server receives them, so bytes moved prove nothing about the endpoint.
// A server that takes the connection and never answers, or drops it part-way
// through a body, acknowledges nothing; the phase must say so rather than
// return nil beside a rate of N/A.
func TestUploadFailsWhenNothingIsAcknowledged(t *testing.T) {
	for name, newHandler := range map[string]func(testOver <-chan struct{}) http.HandlerFunc{
		// The silent handler holds its request until the test is over: the
		// client abandoning a request does not always close the connection
		// under it, and httptest.Server.Close waits for every handler.
		"silent": func(testOver <-chan struct{}) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					return
				}
				select {
				case <-r.Context().Done():
				case <-testOver:
				}
			}
		},
		"reset": func(<-chan struct{}) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					return
				}
				_, _ = io.CopyN(io.Discard, r.Body, 1024)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("Hijack: %v", err)
					return
				}
				_ = conn.Close()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			testOver := make(chan struct{})
			origin := httptest.NewServer(newHandler(testOver))
			t.Cleanup(origin.Close)
			// Cleanups run last-registered first, so the handler is released
			// before Close waits for it.
			t.Cleanup(func() { close(testOver) })
			target := newTestTarget(t, origin.URL, 300*time.Millisecond, 1)

			err := target.UploadTestContext(context.Background())
			// The case under test is bytes handed to the socket with nothing
			// acknowledged; a fixture that misses either half proves nothing.
			if moved := target.Context.GetTotalUpload(); moved == 0 {
				t.Fatalf("the fixture moved no bytes, so it does not reach the case under test")
			}
			if target.Context.GetAckedUploadRate() != 0 {
				t.Fatalf("the fixture acknowledged an upload, so it does not reach the case under test")
			}
			if !errors.Is(err, ErrConnectTimeout) {
				t.Fatalf("an upload phase with nothing acknowledged returned %v, want ErrConnectTimeout", err)
			}
		})
	}
}
