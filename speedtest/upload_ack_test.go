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

// TestUploadExcludesRoundTrip pins the correction for the larger of the two
// errors found on a real path: a worker sends nothing while it waits to be told
// the body arrived, so charging that wait to the link understates it by an
// amount that grows with distance — 19% at 335 ms. The same device would then
// read differently against two endpoints purely because of where they are.
func TestUploadExcludesRoundTrip(t *testing.T) {
	m := newUploadAckMeter()
	m.setLatency(300 * time.Millisecond)
	m.noteWrite(time.Now(), 12*time.Second)

	// 200 kB acknowledged 2.3 s after the first byte, 0.3 s of which was the
	// round trip: the link carried 200 kB in 2 s.
	m.ack(time.Now().Add(-2300*time.Millisecond), 200_000)

	const want = 100_000.0 // bytes/sec
	if got := m.rate(1); got < want*0.98 || got > want*1.02 {
		t.Errorf("rate %.0f B/s, want %.0f — the round trip is being charged to the link", got, want)
	}
}

// TestUploadIgnoresShortRequests pins the correction for the other error: on a
// reused connection the next request's bytes are already in the peer's receive
// buffer when the server turns to read them, so a request no larger than the
// queue appears to complete at an impossible rate. One such sample landing
// alone in a phase reported more than twice the link rate on a real path.
func TestUploadIgnoresShortRequests(t *testing.T) {
	m := newUploadAckMeter()
	m.noteWrite(time.Now(), 12*time.Second)

	// A request that ran long enough for transfer to dominate: 100 kB/s.
	m.ack(time.Now().Add(-2*time.Second), 200_000)
	// A short one that measured the queue rather than the link: 750 kB/s.
	m.ack(time.Now().Add(-100*time.Millisecond), 75_000)

	const want = 100_000.0 // bytes/sec, from the qualifying request alone
	if got := m.rate(1); got > want*1.05 {
		t.Errorf("rate %.0f B/s, want about %.0f — the queue-inflated request is being counted", got, want)
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
	m.noteWrite(time.Now(), 12*time.Second)

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
	// A phase already sized up on a fast link, with one second left to run.
	m.payload = 64 << 20
	m.lastRate = 4 << 20 // bytes/sec
	m.deadline = time.Now().Add(time.Second)

	if got, max := m.nextPayload(), int64(2<<20); got > max {
		t.Errorf("payload %d ignores that only a second of the window is left", got)
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
	_, _ = io.Copy(io.Discard, conn)
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
