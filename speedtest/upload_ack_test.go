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
// customer, while the rate must stay at nothing — and the phase must fail,
// since nothing it sent is known to have arrived.
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

	if err := server.UploadTestContext(context.Background()); !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("an upload phase with nothing acknowledged returned %v, want ErrConnectTimeout", err)
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

// A server may refuse an upload before reading its body. net/http then returns
// the response while its own goroutine is still writing that body, so the
// request's write span is read while the body writer is still updating it.
// Unsynchronized, that is a data race; this test exists for the race detector,
// which CI runs it under.
func TestUploadRejectedMidBodyReadsTheWriteSpanSafely(t *testing.T) {
	answered := make(chan struct{})
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait for the body to start, then refuse it while the client is still
		// writing: far more than the socket buffers hold is left unread.
		_, _ = r.Body.Read(make([]byte, 1))
		// A complete, empty reply: the client must not have to wait for this
		// handler to return before it can finish reading the response.
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusUnauthorized)
		w.(http.Flusher).Flush()
		close(answered)
		<-release
	}))
	defer origin.Close()
	defer close(release)

	client := New()
	client.SetNThread(1)
	server, err := client.CustomServer(origin.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	dm := client.Manager.(*DataManager)
	// The body writer only writes inside a running phase, and the body has to
	// outlast the refusal for the two to overlap.
	dm.runningRW.Lock()
	dm.running = true
	dm.runningRW.Unlock()
	defer func() {
		dm.runningRW.Lock()
		dm.running = false
		dm.runningRW.Unlock()
	}()
	dm.upload.ackMeter.mu.Lock()
	dm.upload.ackMeter.payload = 64 << 20
	dm.upload.ackMeter.mu.Unlock()

	if err := uploadRequest(context.Background(), server); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("a refused upload returned %v, want ErrRequestRejected", err)
	}
	select {
	case <-answered:
	default:
		t.Fatal("the refusal was not sent while the body was being written; the overlap under test never happened")
	}
	// The refusal path records the request as abandoned only when it saw a
	// write start, which proves the span was read after the writer began.
	dm.upload.ackMeter.mu.Lock()
	abandoned := len(dm.upload.ackMeter.abandoned)
	dm.upload.ackMeter.mu.Unlock()
	if abandoned != 1 {
		t.Fatalf("the refused request left %d abandoned entries, want 1", abandoned)
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

// When every connection waits at once, nothing is delivered for that time, and
// delivered throughput has to show it. Leaving those seconds out reported what
// the link could carry while busy, which is not what the test delivered and
// not what the download side reports.
func TestAckMeterChargesTimeWhenEveryWorkerWaits(t *testing.T) {
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

	// Two requests' worth per transfer-plus-wait cycle.
	want := 2 * float64(n) / (transfer + rtt).Seconds()
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f — the shared wait is not being charged to the wall clock", got, want)
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

// Short requests carry real bytes. They are too short to say how fast the link
// is on their own, but what they delivered was delivered: leaving their bytes
// out while their seconds stay in the window read 0.83x in the lab, where the
// last seconds of a phase are full of requests sized down to fit the window.
func TestAckMeterCountsShortRequestsBytes(t *testing.T) {
	m := newUploadAckMeter()
	base := time.Now()
	// One connection carries a single long request...
	m.ackAt(base, base.Add(2*time.Second), 200_000)
	// ...while another carries short ones back to back over the same time,
	// each well under minQualifiedTransfer.
	for i := 0; i < 8; i++ {
		from := base.Add(time.Duration(i) * 250 * time.Millisecond)
		m.ackAt(from, from.Add(250*time.Millisecond), 25_000)
	}

	const want = 400_000.0 / 2 // bytes/sec: both connections' bytes over 2 s
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f — the short requests' bytes are being left out", got, want)
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
	want := 2 * float64(n) / (transfer + rtt).Seconds()
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

	want := 2 * float64(n) / (transfer + rtt).Seconds()
	if got := m.rate(); got < want*0.98 || got > want*1.02 {
		t.Fatalf("rate %.0f B/s, want %.0f from the two connections whose traffic is known", got, want)
	}
}

// The first requests are small by design and spend most of their time on round
// trips, so the start of a phase carries far less than the link can. The window
// opens with the first request long enough to measure the link; opening it at
// the first request read 0.83x in the lab with eight connections at 250 ms.
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
	// Steady state: 200 kB per transfer-plus-wait cycle.
	steady := start
	replayWorker(m, steady, rtt, 2*time.Second, 200_000, 4)

	want := 200_000 / (2*time.Second + rtt).Seconds()
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

	// received is the server's own count of body bytes over time: the truth
	// the measurement is checked against.
	recvMu   sync.Mutex
	received []receivedAt
}

type receivedAt struct {
	at    time.Time
	total int64
}

func (o *pacedOrigin) count(n int) {
	o.recvMu.Lock()
	defer o.recvMu.Unlock()
	var total int64
	if len(o.received) > 0 {
		total = o.received[len(o.received)-1].total
	}
	o.received = append(o.received, receivedAt{time.Now(), total + int64(n)})
}

// totalReceived reports every body byte the server has read.
func (o *pacedOrigin) totalReceived() int64 {
	o.recvMu.Lock()
	defer o.recvMu.Unlock()
	if len(o.received) == 0 {
		return 0
	}
	return o.received[len(o.received)-1].total
}

// deliveredBetween reports the bytes per second the server received in [from, to].
func (o *pacedOrigin) deliveredBetween(from, to time.Time) float64 {
	o.recvMu.Lock()
	defer o.recvMu.Unlock()
	at := func(t time.Time) int64 {
		var total int64
		for _, r := range o.received {
			if r.at.After(t) {
				break
			}
			total = r.total
		}
		return total
	}
	return float64(at(to)-at(from)) / to.Sub(from).Seconds()
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
			o.count(n)
		}
		if err != nil {
			break
		}
	}
	time.Sleep(o.rtt / 2)
	w.WriteHeader(http.StatusOK)
}

// The end-to-end form of the meter tests: with eight connections sharing one
// capped link, the reading must match what the server received per second over
// the steady part of the phase, whatever the round trip.
//
// The testbed paces reads in user space, so its connections tend to fall into
// step and leave the link idle together; the server's own count, not the cap,
// is therefore the truth. Per-worker averaging read high against it and
// increasingly so with distance, which is the regression this pins.
//
// It stops at 400 ms. At 1 s an eight-second phase holds two or three request
// cycles and the server's own per-second rate swings between 0.8 and 4.2 MB/s,
// so there is no steady part to compare with; long round trips are covered by
// the lab in box/tools/speedtest-lab, over a real TCP stack.
func TestUploadRateDoesNotDriftWithRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("runs two eight-second phases")
	}
	const capBytesPerSec = 4 << 20 // 33.5 Mbps
	for _, rtt := range []time.Duration{0, 400 * time.Millisecond} {
		t.Run(rtt.String(), func(t *testing.T) {
			paced := &pacedOrigin{bytesPerSec: capBytesPerSec, rtt: rtt}
			origin := httptest.NewServer(paced)
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

			start := time.Now()
			if err := server.UploadTestContext(context.Background()); err != nil {
				t.Fatalf("UploadTestContext: %v", err)
			}
			end := time.Now()
			got := float64(server.ULSpeed)
			want := paced.deliveredBetween(start.Add(2*time.Second), end)
			if lo, hi := want*0.9, want*1.1; got < lo || got > hi {
				t.Errorf("upload rate %.0f B/s at a %v round trip, want within [%.0f, %.0f] of the %.0f B/s the server received (%.2fx)",
					got, rtt, lo, hi, want, got/want)
			}
		})
	}
}

// A request still in flight when the capture window closes has usually been
// read by the server already; only its acknowledgement is on the way. Cancelling
// it at once threw that away: in the lab a single lossy connection's last
// request — 2.9 MB the server logged as received — went unacknowledged, the rate
// window shrank to the 1.1 s before it, and the reading came out 3x what the
// phase delivered. The phase now waits a bounded grace for those answers, so
// every byte the server received is accounted for.
func TestUploadWaitsForInFlightAcknowledgements(t *testing.T) {
	const rtt = 600 * time.Millisecond
	paced := &pacedOrigin{bytesPerSec: 1 << 20, rtt: rtt}
	origin := httptest.NewServer(paced)
	defer origin.Close()

	client := New()
	client.SetCaptureTime(2500 * time.Millisecond)
	client.SetNThread(1)
	server, err := client.CustomServer(origin.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	server.Latency = rtt
	if err := server.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("UploadTestContext: %v", err)
	}

	meter := client.Manager.(*DataManager).upload.ackMeter
	meter.mu.Lock()
	var acked int64
	for _, span := range meter.acked {
		acked += span.n
	}
	abandoned := len(meter.abandoned)
	meter.mu.Unlock()
	received := paced.totalReceived()

	if abandoned != 0 {
		t.Errorf("%d request(s) were abandoned at the end of the phase instead of being waited for", abandoned)
	}
	if acked != received {
		t.Errorf("acknowledged %d bytes but the server received %d — some of what it received is unaccounted for", acked, received)
	}
}

// A short request measures the start of a transfer — the initial window, a
// queue still empty — not what the link sustains, so it may only nudge the next
// request's size. In the lab a 1 MiB request that finished in 0.43 s sized the
// next at 4.9 MB; on a lossy link that request took 11 s, outlived the capture
// window and its grace, and left the phase with nothing but its ramp to measure.
func TestUploadPayloadGrowthFromShortRequestIsBounded(t *testing.T) {
	const rtt = 110 * time.Millisecond
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	m.noteWrite(base, 10*time.Second)
	m.ackAt(base, base.Add(431*time.Millisecond+rtt), 1<<20)

	if got, max := m.nextPayload(), int64(maxUnqualifiedGrowth<<20); got > max {
		t.Errorf("a %v request sized the next at %d bytes; growth from a short request is capped at %d",
			431*time.Millisecond, got, max)
	}
}

// The grace after the capture window has to cover a request slowed by loss, not
// only an acknowledgement's round trip: one that finished 1 s past a grace of
// 1 s plus two round trips was thrown away in the lab.
func TestUploadAckGraceCoversAQuarterOfTheWindow(t *testing.T) {
	cases := []struct {
		latency, capture, want time.Duration
	}{
		{110 * time.Millisecond, 10 * time.Second, 2500 * time.Millisecond},
		{2 * time.Second, 4 * time.Second, 5 * time.Second}, // round trips dominate
	}
	for _, c := range cases {
		if got := uploadAckGrace(c.latency, c.capture); got != c.want {
			t.Errorf("grace for latency %v, capture %v = %v, want %v", c.latency, c.capture, got, c.want)
		}
	}
}

// slowAckOrigin reads each upload body as it arrives and answers a fixed delay
// after the last byte: a server, or a path, slow to acknowledge.
type slowAckOrigin struct{ delay time.Duration }

func (o slowAckOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(o.delay)
	}
	w.WriteHeader(http.StatusOK)
}

// The window share of the grace has to reach the phase itself, not only the
// helper that computes it. Here the latency phase measured a short round trip,
// but every answer comes 1.3 s after the body: a grace of a second plus two
// round trips (1.1 s) gives up on the request in flight at the close, a quarter
// of the 6 s window (1.5 s) does not.
func TestUploadGraceUsesTheCaptureWindow(t *testing.T) {
	origin := httptest.NewServer(slowAckOrigin{delay: 1300 * time.Millisecond})
	defer origin.Close()

	client := New()
	client.SetCaptureTime(6 * time.Second)
	client.SetNThread(1)
	server, err := client.CustomServer(origin.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	server.Latency = 50 * time.Millisecond
	if err := server.UploadTestContext(context.Background()); err != nil {
		t.Fatalf("UploadTestContext: %v", err)
	}

	meter := client.Manager.(*DataManager).upload.ackMeter
	meter.mu.Lock()
	abandoned := len(meter.abandoned)
	meter.mu.Unlock()
	if abandoned != 0 {
		t.Errorf("%d request(s) were given up on although every answer came within a quarter of the window", abandoned)
	}
}

// Both upload entry points take their grace from the client's own capture
// window; a grace computed without it falls back to the round-trip bound alone.
func TestUploadPhaseGraceUsesTheClientsCaptureWindow(t *testing.T) {
	client := New()
	client.SetCaptureTime(10 * time.Second)
	server, err := client.CustomServer("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	server.Latency = 110 * time.Millisecond
	if got, want := uploadPhaseGrace(server), 2500*time.Millisecond; got != want {
		t.Fatalf("upload phase grace %v, want %v from a quarter of the 10 s window", got, want)
	}
}

// On a long path slow start keeps even a small request in flight for several
// round trips, so transfer time alone can call a ramp request qualified. In the
// lab a 64 KiB request took 0.54 s at 250 ms, cleared a half-second threshold,
// and opened the window while the link was still at a fifth of its rate:
// 0.81 of what the second half of the phase delivered.
func TestAckMeterSlowStartRequestDoesNotOpenTheWindow(t *testing.T) {
	const rtt = 250 * time.Millisecond
	m := newUploadAckMeter()
	m.setLatency(rtt)
	base := time.Now()
	// Ramp: small requests, each mostly round trips and slow start, carrying
	// about a third of the steady rate — while their transfer times, 0.54 s
	// and 0.8 s, clear a half-second threshold.
	m.ackAt(base, base.Add(rtt+540*time.Millisecond), 32<<10)
	start := base.Add(rtt + 540*time.Millisecond)
	m.ackAt(start, start.Add(rtt+800*time.Millisecond), 64<<10)
	// Steady state: 1.25 s transfers of 250 kB, 200 kB/s while sending.
	steady := start.Add(rtt + 800*time.Millisecond)
	replayWorker(m, steady, rtt, 1250*time.Millisecond, 250_000, 6)

	want := 250_000 / (1250*time.Millisecond + rtt).Seconds()
	if got := m.rate(); got < want*0.97 || got > want*1.03 {
		t.Fatalf("rate %.0f B/s, want %.0f — a slow-start request opened the window", got, want)
	}
}

// Every server on a client shares one upload meter, and each upload phase
// supplies its own server's round trip before it starts. A phase against a
// server whose latency was never measured must apply no correction, not the
// round trip an earlier phase left behind on another path.
func TestUploadLatencyIsNotInheritedFromAnEarlierPhase(t *testing.T) {
	dm := NewDataManager()
	dm.SetUploadLatency(200 * time.Millisecond)
	dm.upload.ackMeter.reset()

	dm.SetUploadLatency(0)

	dm.upload.ackMeter.mu.Lock()
	latency := dm.upload.ackMeter.latency
	dm.upload.ackMeter.mu.Unlock()
	if latency != 0 {
		t.Fatalf("a phase with no measured latency applied %v from the previous phase", latency)
	}
}

// One meter serves every server in a multi-server upload, and it can subtract
// only one round trip. Charging every request the main server's would credit a
// near server with time it never spent and a far one with time it did, so with
// servers on different paths no correction is applied. A single server keeps
// its own.
func TestMultiUploadAppliesOneServersLatencyOnlyToItself(t *testing.T) {
	server, _ := newCountingServer(t, nil)
	far := newTestTarget(t, server.URL, 300*time.Millisecond, 2)
	near, err := far.Context.CustomServer(server.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	far.Latency = 200 * time.Millisecond
	near.Latency = 5 * time.Millisecond
	meter := far.Context.Manager.(*DataManager).upload.ackMeter
	latency := func() time.Duration {
		meter.mu.Lock()
		defer meter.mu.Unlock()
		return meter.latency
	}

	if err := far.MultiUploadTestContext(context.Background(), Servers{far, near}); err != nil {
		t.Fatalf("MultiUploadTestContext: %v", err)
	}
	if got := latency(); got != 0 {
		t.Fatalf("servers on different paths were all charged a round trip of %v", got)
	}

	if err := far.MultiUploadTestContext(context.Background(), Servers{far}); err != nil {
		t.Fatalf("MultiUploadTestContext: %v", err)
	}
	if got := latency(); got != far.Latency {
		t.Fatalf("a single server was charged %v instead of its own %v", got, far.Latency)
	}
}
