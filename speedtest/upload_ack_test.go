package speedtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// shapedOrigin serves an endpoint that reads request bodies at a fixed rate and
// counts what it received, so a test has a wire rate to compare against rather
// than a second client-side number.
type shapedOrigin struct {
	bytesPerSec int64
}

const shaperSlice = 10 * time.Millisecond

func (o *shapedOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK) // ping
		return
	}
	perSlice := o.bytesPerSec * int64(shaperSlice) / int64(time.Second)
	buf := make([]byte, perSlice)
	next := time.Now()
	for {
		next = next.Add(shaperSlice)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			break
		}
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// TestUploadRateMatchesWireRate checks the measurement end to end against an
// origin that meters what it reads.
//
// It is a smoke test, not the regression test for the write-boundary defect:
// over loopback there is no bandwidth-delay product, so the send queue that
// defect reports as throughput barely exists here. See
// TestUploadRateIgnoresUnacknowledgedBytes for the property that actually
// pins it.
func TestUploadRateMatchesWireRate(t *testing.T) {
	const wireBytesPerSec = 128 << 10 // 1.05 Mbps, well inside the old defect's range

	origin := httptest.NewServer(&shapedOrigin{bytesPerSec: wireBytesPerSec})
	defer origin.Close()

	client := New()
	client.SetCaptureTime(6 * time.Second)
	// The origin shapes each connection independently, so the wire rate is only
	// the configured one while a single connection is in flight.
	client.SetNThread(1)
	server, err := client.CustomServer(origin.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}

	if err := server.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("UploadTestContext: %v", err)
	}

	got := float64(server.ULSpeed)
	// The shaper meters in slices and the ramp costs part of the window, so the
	// measurement is allowed to sit a little low; it is not allowed to run high,
	// which is the failure this test exists for.
	lo, hi := wireBytesPerSec*0.7, wireBytesPerSec*1.2
	if got < lo || got > hi {
		t.Errorf("upload rate %.0f B/s, want within [%.0f, %.0f] of the %d B/s wire rate",
			got, lo, hi, int64(wireBytesPerSec))
	}
}

// TestUploadPayloadRampIsBounded pins the sizing regression found while
// building this: the first request is small enough that its duration is all
// overhead, so the rate derived from it can be orders of magnitude too high. An
// unbounded ramp then sizes a request that cannot finish inside the window, and
// a request that is never acknowledged contributes nothing — leaving the phase
// with only the bad sample it started from.
func TestUploadPayloadRampIsBounded(t *testing.T) {
	m := newUploadAckMeter()
	start := time.Now()
	m.noteWrite(start, 12*time.Second)

	// A minimum-size request that appeared to complete instantly.
	m.ack(time.Now(), minUploadPayload)

	if got, max := m.nextPayload(), int64(minUploadPayload*maxGrowthFactor); got > max {
		t.Errorf("payload grew to %d from one overhead-dominated sample, capped at %d", got, max)
	}
}

// TestUploadPayloadFitsRemainingWindow pins the other half of that regression:
// even a well-founded rate must not size a request past the end of the capture
// window, or it is cancelled before the server can acknowledge it.
func TestUploadPayloadFitsRemainingWindow(t *testing.T) {
	m := newUploadAckMeter()
	start := time.Now()
	m.noteWrite(start, time.Second)

	// 8 MiB/s observed over a full second: the unclamped payload would be 16 MiB,
	// which cannot complete in the fraction of a second still left.
	m.ack(start, 8<<20)

	got := m.nextPayload()
	if got > 8<<20 {
		t.Errorf("payload %d ignores the %v left in the window", got, time.Until(start.Add(time.Second)))
	}
}

// hangingOrigin takes over the connection and drains it without ever writing a
// response, so every byte it reads is a byte the client sent and the server
// never acknowledged. Returning from a normal handler would not do: net/http
// answers 200 on the way out, which is an acknowledgement.
type hangingOrigin struct{}

func (o *hangingOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK) // ping
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("test server does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	io.Copy(io.Discard, conn)
}

// TestUploadRateIgnoresUnacknowledgedBytes is the regression test for the
// defect this mechanism replaced.
//
// Counting bytes where they are handed to the transport cannot tell "sent" from
// "queued", so it reports throughput for bytes that never reached anyone. Here
// the server reads every byte and never acknowledges any: the traffic counter
// must still see them, because they really were sent and really do cost the
// customer, while the rate must stay at nothing.
func TestUploadRateIgnoresUnacknowledgedBytes(t *testing.T) {
	origin := httptest.NewServer(&hangingOrigin{})
	defer origin.Close()

	client := New()
	client.SetCaptureTime(time.Second)
	client.SetNThread(1)
	server, err := client.CustomServer(origin.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}

	if err := server.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("UploadTestContext: %v", err)
	}

	if moved := client.GetTotalUpload(); moved <= 0 {
		t.Fatalf("no bytes were sent, so the test never exercised the path")
	}
	if got := float64(server.ULSpeed); got > 0 {
		t.Errorf("reported %.0f B/s of upload throughput although the server acknowledged nothing", got)
	}
}
