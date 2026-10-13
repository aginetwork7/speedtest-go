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
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Errorf("rate %.0f B/s, want %.0f — the round trip is being charged to the link", got, want)
	}
}

// TestUploadIgnoresShortRequests pins the correction for the other error: on a
// reused connection the next request's bytes are already in the peer's receive
// buffer when the server turns to read them, so a request no larger than the
// queue appears to complete at an impossible rate. One such sample landing
// alone in a phase reported more than twice the link rate on a real path.
//
// The requests here run one after another on one connection, as they do in a
// phase: a short request between two long ones must not lift the reading above
// what the link carried.
func TestUploadIgnoresShortRequests(t *testing.T) {
	const rtt = 300 * time.Millisecond
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	m.noteWrite(base, 12*time.Second)

	// 200 kB in 2 s of transfer, then the round trip: 100 kB/s on the wire.
	m.ackAt(base, base.Add(2300*time.Millisecond), 200_000)
	// A short one acknowledged almost as soon as the round trip allows, which
	// on its own reads as 600 kB/s.
	m.ackAt(base.Add(2300*time.Millisecond), base.Add(2650*time.Millisecond), 30_000)
	m.ackAt(base.Add(2650*time.Millisecond), base.Add(4950*time.Millisecond), 200_000)

	const link = 100_000.0 // bytes/sec
	if got := m.rate(); got > link*1.05 {
		t.Errorf("rate %.0f B/s, want at most about %.0f — the queue-inflated request is being counted", got, link)
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

// The bound is per request, not per phase. Every worker opens with a
// minimum-size request, so with eight workers eight overhead-dominated samples
// arrive almost together. Bounding growth by the shared payload let each one
// multiply what the previous one had already multiplied — 16 KiB, 128 KiB,
// 1 MiB, 8 MiB within a quarter of a second — which sized a request that one
// connection's share of the link could not finish inside the window. It was
// never acknowledged, and its bytes were measured by nothing.
func TestUploadPayloadRampIsBoundedPerRequest(t *testing.T) {
	m := newUploadAckMeter()
	m.noteWrite(time.Now(), 12*time.Second)

	for i := 0; i < 8; i++ {
		m.ack(time.Now(), minUploadPayload)
	}

	if got, max := m.nextPayload(), int64(minUploadPayload*maxGrowthFactor); got > max {
		t.Errorf("eight first requests grew the payload to %d; each may only size from its own %d bytes, capped at %d",
			got, minUploadPayload, max)
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
	// The request the phase cancelled has to be on record: it is what closes
	// the rate's window, and without it the wait after the last acknowledged
	// request drops out of the measurement.
	meter := client.Manager.(*DataManager).upload.ackMeter
	meter.mu.Lock()
	abandoned := len(meter.abandoned)
	meter.mu.Unlock()
	if abandoned == 0 {
		t.Fatal("the request cancelled at the end of the phase was not recorded as abandoned")
	}
}

// A response that rejects the request proves nothing about the link: an error
// status can be written before the body has been read at all, so counting it
// would credit the link with bytes it never carried.
//
// A phase whose every request was refused has to say so. Reporting it as a
// phase that merely produced no rate sent an operator looking at the link,
// when the server had no upload route at all and answered every request 404.
func TestUploadDoesNotAcknowledgeRejectedRequests(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK) // ping
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer origin.Close()

	client := New()
	client.SetCaptureTime(500 * time.Millisecond)
	client.SetNThread(1)
	server, err := client.CustomServer(origin.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}

	if err := server.UploadTestContext(context.Background()); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("a phase whose every upload was refused returned %v, want ErrRequestRejected", err)
	}
	if rate := client.GetAckedUploadRate(); rate != 0 {
		t.Fatalf("rejected requests were counted as acknowledged: %v B/s", rate)
	}
	if server.ULSpeed != -1 {
		t.Fatalf("a phase whose every request was rejected must read N/A, got %v", server.ULSpeed)
	}
	// With one worker, closing the phase cancels at most one request; every
	// other abandoned entry can only have come from a refusal.
	meter := client.Manager.(*DataManager).upload.ackMeter
	meter.mu.Lock()
	abandoned := len(meter.abandoned)
	meter.mu.Unlock()
	if abandoned <= 1 {
		t.Fatalf("refused uploads were not recorded as abandoned (%d on record)", abandoned)
	}
	if err := uploadRequest(context.Background(), server); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("a refused upload returned %v, want ErrRequestRejected", err)
	}
}

// ack() is fed straight from WriteSpan(), which reports a zero time and zero
// bytes for a chunk that never wrote. Those carry no duration to measure.
func TestAckMeterIgnoresUnusableSpans(t *testing.T) {
	meter := newUploadAckMeter()

	meter.ack(time.Time{}, 1<<20)
	meter.ack(time.Now().Add(-time.Second), 0)

	if rate := meter.rate(); rate != 0 {
		t.Fatalf("a meter with no usable sample reported %v B/s", rate)
	}
	if payload := meter.nextPayload(); payload != minUploadPayload {
		t.Fatalf("request sizing moved on an unusable sample: %d", payload)
	}
}

// worker replays one connection's requests on a meter: each request transfers
// for transfer, then waits one round trip for its acknowledgement, and the next
// request starts when that acknowledgement arrives. The final request is still
// in flight when the phase ends, so it is abandoned rather than acknowledged.
func replayWorker(m *uploadAckMeter, base time.Time, rtt, transfer time.Duration, n int64, requests int) {
	start := base
	for i := 0; i < requests; i++ {
		at := start.Add(transfer + rtt)
		m.ackAt(start, at, n)
		start = at
	}
	// Cancelled a moment after it began, as the phase closes.
	m.abandonAt(start, start.Add(100*time.Millisecond))
}

// The regression this file's rate exists to prevent: a connection sends
// nothing while it waits for an acknowledgement, and on a shared link another
// connection carries the traffic meanwhile. Dividing summed per-request
// transfer time by the number of workers assumed every worker was always
// sending, so each wait was credited a second time as throughput: 1.08-1.19
// times the cap on a testbed at 400 ms and 1 s, growing with distance.
//
// Here the two connections are offset by half a cycle, as they drift apart on
// a real path, so one is always sending while the other waits.
func TestAckMeterDoesNotCreditOneWorkersWaitToAnother(t *testing.T) {
	const (
		rtt      = 400 * time.Millisecond
		transfer = 2 * time.Second
		n        = 200_000
	)
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	replayWorker(m, base, rtt, transfer, n, 4)
	replayWorker(m, base.Add((transfer+rtt)/2), rtt, transfer, n, 4)

	// The window runs from 0.2 s to 9.8 s, where the first connection's
	// abandoned request began, and someone is sending throughout it. The first
	// connection's four requests and the second's first three fall inside it,
	// plus 0.6 of the second's fourth.
	const want = (7*n + 0.6*n) / 9.6
	legacy := 2 * float64(n) / transfer.Seconds()
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f — a wait is being credited as throughput (per-worker averaging reads %.0f)", got, want, legacy)
	}
}

// When every connection waits at once, the link is idle because of how the
// test drives it, not because it is slow; that time says nothing about its
// capacity. Charging it read 0.79-0.84 of the cap on a 1 s testbed where the
// workers had fallen into step.
func TestAckMeterExcludesTimeWhenEveryWorkerWaits(t *testing.T) {
	const (
		rtt      = time.Second
		transfer = 2 * time.Second
		n        = 200_000
	)
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	replayWorker(m, base, rtt, transfer, n, 3)
	replayWorker(m, base, rtt, transfer, n, 3)

	want := 2 * float64(n) / transfer.Seconds()
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f — time when nobody was sending is being charged to the link", got, want)
	}
}

// Concurrent requests carry bytes in the same wall-clock seconds, so they add
// up rather than averaging. Production runs with more than one connection.
func TestAckMeterAddsConcurrentRequests(t *testing.T) {
	base := time.Now()
	single := newUploadAckMeter()
	single.ackAt(base, base.Add(2*time.Second), 2<<20)

	paired := newUploadAckMeter()
	paired.ackAt(base, base.Add(2*time.Second), 2<<20)
	paired.ackAt(base, base.Add(2*time.Second), 2<<20)

	if single.rate() <= 0 {
		t.Fatal("no rate from an acknowledged request")
	}
	if ratio := paired.rate() / single.rate(); ratio < 1.98 || ratio > 2.02 {
		t.Fatalf("two concurrent requests should read twice one, got %.2fx", ratio)
	}
}

// Bytes of a request that was never acknowledged are unknown, so the window
// must close before such a request began. Measuring past it counts seconds
// whose traffic is missing from the numerator, and reads low by however much
// of the window it covers.
func TestAckMeterWindowEndsAtTheFirstUnacknowledgedRequest(t *testing.T) {
	const (
		rtt      = 200 * time.Millisecond
		transfer = 2 * time.Second
		n        = 200_000
	)
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	// One connection keeps being acknowledged for five requests; the other is
	// abandoned after two, while the first is still going.
	replayWorker(m, base, rtt, transfer, n, 5)
	replayWorker(m, base, rtt, transfer, n, 2)

	// Both connections send in step until the second is abandoned; past that
	// point only the first one's traffic is known.
	want := 2 * float64(n) / transfer.Seconds()
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f — the window ran past a request whose bytes were never counted", got, want)
	}
}

// A request that has been in flight far longer than any request is sized to
// take — a link that slowed mid-phase, or a sizing mistake — must not drag the
// window back to where it began. Its bytes stay unknown, which costs at most
// that connection's share; closing the window at its start instead read 0.10
// of the cap in a testbed phase where one request overran from the first
// quarter second.
func TestAckMeterOverrunDoesNotCollapseTheWindow(t *testing.T) {
	const (
		rtt      = 250 * time.Millisecond
		transfer = 2 * time.Second
		n        = 200_000
	)
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	// The ramp: every connection's first, minimum-size request.
	for i := 0; i < 3; i++ {
		m.ackAt(base, base.Add(rtt+20*time.Millisecond), minUploadPayload)
	}
	steady := base.Add(rtt + 20*time.Millisecond)
	replayWorker(m, steady, rtt, transfer, n, 4)
	replayWorker(m, steady, rtt, transfer, n, 4)
	// The third connection's second request overran from the end of the ramp,
	// before any request had qualified, to the end of the phase — as in the
	// testbed phase this pins.
	m.abandonAt(steady, steady.Add(4*(transfer+rtt)))

	want := 2 * float64(n) / transfer.Seconds()
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f from the two connections whose traffic is known", got, want)
	}
}

// The first requests are small by design and spend most of their time on round
// trips, so the start of a phase carries far less than the link can, and the
// little they carry is credited to an interval the queue compressed. They do
// not qualify, so they count towards neither the bytes nor the time.
func TestAckMeterWindowSkipsTheRamp(t *testing.T) {
	const rtt = 300 * time.Millisecond
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	start := base
	// Three ramp requests: tiny bodies, each mostly a round trip.
	for i := 0; i < 3; i++ {
		at := start.Add(rtt + 50*time.Millisecond)
		m.ackAt(start, at, 16<<10)
		start = at
	}
	// Steady state: 100 kB/s while sending.
	steady := start
	replayWorker(m, steady, rtt, 2*time.Second, 200_000, 4)

	want := 200_000 / (2 * time.Second).Seconds()
	if got := m.rate(); got < want*0.97 || got > want*1.03 {
		t.Fatalf("rate %.0f B/s, want %.0f — the ramp is being averaged into the steady rate", got, want)
	}
}

// A phase that ended before any request qualified still reports what it saw,
// measured over the wall clock it took: low, never high.
func TestAckMeterFallsBackToTheWholeAcknowledgedSpan(t *testing.T) {
	m := newUploadAckMeter()
	base := time.Now()
	m.ackAt(base, base.Add(500*time.Millisecond), 50_000)
	m.ackAt(base.Add(500*time.Millisecond), base.Add(time.Second), 50_000)

	const want = 100_000.0
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f from the acknowledged span alone", got, want)
	}
}

// A phase measures itself. The meter outlives the phase, so what it collected
// has to be cleared when the next one starts — except the latency, which
// belongs to the path and is supplied before the phase begins.
func TestAckMeterResetKeepsLatencyOnly(t *testing.T) {
	meter := newUploadAckMeter()
	meter.setLatency(50 * time.Millisecond)
	meter.ack(time.Now().Add(-2*time.Second), 4<<20)
	meter.abandon(time.Now())
	if meter.rate() == 0 {
		t.Fatal("no rate to clear")
	}

	meter.reset()

	if rate := meter.rate(); rate != 0 {
		t.Fatalf("the previous phase's samples survived the reset: %v B/s", rate)
	}
	if len(meter.abandoned) != 0 {
		t.Fatalf("the previous phase's abandoned requests survived the reset: %d", len(meter.abandoned))
	}
	if meter.payload != minUploadPayload {
		t.Fatalf("request sizing survived the reset: %d", meter.payload)
	}
	if meter.latency != 50*time.Millisecond {
		t.Fatalf("the measured round trip was cleared: %v", meter.latency)
	}
}

// pacedOrigin caps the upload rate of all connections together and emulates a
// round trip of rtt. A shared cap is what a real access link is: a connection
// waiting for its acknowledgement leaves its share to the others, so the link
// stays full while individual connections sit idle. The cap is the truth the
// measurement is checked against.
//
// The round trip is split the way a path splits it: half before the first body
// byte can be read, half after the last before the response arrives, so that
// the server receives each request where the meter places it.
type pacedOrigin struct {
	bytesPerSec float64
	rtt         time.Duration

	mu   sync.Mutex
	next time.Time
}

// wait blocks until n more bytes fit under the shared cap.
func (o *pacedOrigin) wait(n int) {
	o.mu.Lock()
	now := time.Now()
	if o.next.Before(now) {
		o.next = now
	}
	o.next = o.next.Add(time.Duration(float64(n) / o.bytesPerSec * float64(time.Second)))
	until := o.next
	o.mu.Unlock()
	time.Sleep(time.Until(until))
}

func (o *pacedOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		time.Sleep(o.rtt) // latency probe: the whole round trip
		w.WriteHeader(http.StatusOK)
		return
	}
	time.Sleep(o.rtt / 2)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			o.wait(n)
		}
		if err != nil {
			break
		}
	}
	time.Sleep(o.rtt / 2)
	w.WriteHeader(http.StatusOK)
}

// The end-to-end form of the meter tests: with eight connections sharing one
// capped link, the reading must match the cap whatever the round trip.
//
// Per-worker averaging read 1.08-1.19 times the cap at 400 ms and 1 s over
// three runs each, and this reads 0.94-1.00, so the band is asymmetric: an
// upper bound of 1.05 sits between the two, and the lower bound leaves room
// for a reading that errs low, which is the direction this measurement is
// built to err in. At 0 ms both agree, which keeps the testbed itself honest.
func TestUploadRateDoesNotDriftWithRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("runs three eight-second phases")
	}
	const capBytesPerSec = 4 << 20 // 33.5 Mbps
	for _, rtt := range []time.Duration{0, 400 * time.Millisecond, time.Second} {
		t.Run(rtt.String(), func(t *testing.T) {
			origin := httptest.NewServer(&pacedOrigin{bytesPerSec: capBytesPerSec, rtt: rtt})
			defer origin.Close()

			client := New()
			client.SetCaptureTime(8 * time.Second)
			client.SetNThread(8)
			server, err := client.CustomServer(origin.URL)
			if err != nil {
				t.Fatalf("CustomServer: %v", err)
			}
			// What the latency phase would have measured; the meter needs it to
			// tell transfer from waiting.
			server.Latency = rtt

			if err := server.UploadTestContext(context.Background()); err != nil {
				t.Fatalf("UploadTestContext: %v", err)
			}
			got := float64(server.ULSpeed)
			if lo, hi := capBytesPerSec*0.9, capBytesPerSec*1.05; got < lo || got > hi {
				t.Errorf("upload rate %.0f B/s at a %v round trip, want within [%.0f, %.0f] of the %d B/s cap (%.2fx)",
					got, rtt, lo, hi, capBytesPerSec, got/capBytesPerSec)
			}
		})
	}
}
