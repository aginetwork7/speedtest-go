package speedtest

import (
	"sync"
	"time"
)

// Upload throughput is measured against server acknowledgements, not against
// bytes handed to the transport.
//
// The two are not the same quantity. A download counts bytes returned by Read,
// which have arrived; an upload counting bytes returned by Write counts bytes
// that are merely queued — HTTP buffer, kernel send buffer, bandwidth-delay
// product and the peer's receive window. An HTTP response is the one
// per-request acknowledgement that cannot be inflated that way: the server
// sends it only after reading the whole body.
//
// Three corrections make that signal usable. The first two were found on a
// real 800 kbps path where the uncorrected version read 19% low with occasional
// readings at more than twice the link rate; the third on a 250 ms path, and
// confirmed against a testbed whose server caps the link and counts what it
// reads:
//
//   - The round trip is not transfer time. A worker waits a full round trip
//     between writing its last byte and being told the body arrived, and it
//     sends nothing during that wait. Charging it to the link understates a
//     long path badly — 19% at 335 ms — and by an amount that grows with
//     distance, so the same device reads differently against two endpoints.
//
//   - Short requests measure the queue, not the link. On a reused connection
//     the next request's bytes are already sitting in the peer's receive buffer
//     when the server turns to read them, so a request no larger than the queue
//     appears to complete at an impossible rate — measured at 1463 kbps on that
//     same 800 kbps path. Requests that ran long enough for transfer to
//     dominate are the only ones that measure anything.
//
//   - The rate is delivered throughput: acknowledged bytes per second of wall
//     clock inside a steady window. Two other aggregates were tried and
//     measured against a lab whose server counts what it receives over a real
//     TCP stack: summing per-request transfer time and dividing by the number
//     of workers read up to 4x high on a lossy single connection, because it
//     leaves out every wait between requests; dividing only qualified bytes by
//     the time qualified requests were in flight read 0.78-0.88x with several
//     connections at 250 ms, because its window still covered the ramp. The
//     download side reports bytes read over time, and this is the same
//     quantity.
const (
	// targetRequestDuration is how long one upload request should take. Long
	// enough that per-request overhead stays a small share of it, short enough
	// that a capture window collects several acknowledgements.
	targetRequestDuration = 2 * time.Second

	// minQualifiedTransfer is the transfer time below which a request tells us
	// more about the queue and the round trip than about the link.
	minQualifiedTransfer = targetRequestDuration / 2

	// minUploadPayload bounds how slow a link can be and still have one request
	// acknowledged inside the shortest usable capture window: 16 KiB needs
	// about 13 kbps to complete in 10 seconds. It is also the size of the first
	// request, before any rate is known.
	minUploadPayload = 16 << 10

	// maxUploadPayload caps what a single request may cost on a fast link.
	// 128 MiB is about one second of gigabit.
	maxUploadPayload = 128 << 20

	// maxGrowthFactor bounds how far one request may resize the next, relative
	// to that request's own size. The first
	// request is deliberately small, so its duration is dominated by connection
	// setup and round trips rather than by bandwidth, and the rate derived from
	// it can be off by orders of magnitude. Unbounded, that one sample sizes a
	// request too large to finish inside the capture window — which leaves the
	// phase with nothing but the bad sample it started from.
	maxGrowthFactor = 8

	// overrunFactor is how many times a normal request's lifetime — its target
	// transfer time plus the round trip — an abandoned request must have been
	// in flight to count as an overrun rather than a request the phase simply
	// cut off. Sizing aims every request at targetRequestDuration and at half
	// the remaining window, so twice that is already well outside what it
	// produces; what lands there is a link that slowed mid-phase.
	overrunFactor = 2

	// completionSafetyFactor is the share of the remaining capture window a
	// request may be sized to occupy. A request that outlives the window is
	// cancelled before the server answers, so it is never acknowledged and its
	// bytes are measured by nothing.
	completionSafetyFactor = 0.5
)

// uploadAckMeter sizes upload requests and derives the rate from the ones the
// server acknowledged. It lives as long as its direction, is cleared at the
// start of every phase, and is safe for concurrent use by the phase's workers.
type uploadAckMeter struct {
	mu sync.Mutex

	payload  int64         // body size for the next request
	latency  time.Duration // measured round trip, excluded from transfer time
	lastRate float64       // bytes/sec from the most recent acknowledgement
	deadline time.Time     // when the capture window closes

	// acked holds every acknowledged request as the interval the server spent
	// receiving it.
	acked []ackSpan
	// abandoned holds each request that was never acknowledged. Its bytes are
	// unknown, so no window may extend past where the earliest of them began —
	// unless it overran; see rate().
	abandoned []abandonedSpan
}

// abandonedSpan is one request that was never acknowledged: from is when the
// server began receiving it, at when the client gave up on it.
type abandonedSpan struct {
	from, at time.Time
}

// ackSpan is one acknowledged request, placed on the server's clock.
type ackSpan struct {
	from, to time.Time
	n        int64
	// qualified marks a request long enough to measure the link rather than
	// the queue; see minQualifiedTransfer.
	qualified bool
}

func newUploadAckMeter() *uploadAckMeter {
	return &uploadAckMeter{payload: minUploadPayload}
}

// setLatency supplies the round trip measured by the latency phase. Left at
// zero — no latency phase ran — no correction is applied, which reads low
// rather than inventing a number.
func (m *uploadAckMeter) setLatency(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.latency = d
	}
}

// reset clears what a phase measured. The latency is left in place: it belongs
// to the path, and it is supplied before the phase starts.
func (m *uploadAckMeter) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payload = minUploadPayload
	m.lastRate = 0
	m.deadline = time.Time{}
	m.acked = nil
	m.abandoned = nil
}

// nextPayload reports how large the next request body should be.
func (m *uploadAckMeter) nextPayload() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	payload := m.payload
	// Never ask for more than the remaining window can deliver: an
	// unacknowledged request measures nothing, however many bytes it moved.
	if m.lastRate > 0 && !m.deadline.IsZero() {
		if remaining := time.Until(m.deadline).Seconds(); remaining > 0 {
			if fits := int64(m.lastRate * remaining * completionSafetyFactor); fits < payload {
				payload = fits
			}
		}
	}
	if payload < minUploadPayload {
		payload = minUploadPayload
	}
	return payload
}

// noteWrite records when the phase first put a byte on the wire, which is what
// the capture window is counted from.
func (m *uploadAckMeter) noteWrite(at time.Time, window time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deadline.IsZero() {
		m.deadline = at.Add(window)
	}
}

// ack records that the server acknowledged n bytes from a request that began
// writing at reqStart, and resizes the next request from what that one
// achieved.
func (m *uploadAckMeter) ack(reqStart time.Time, n int64) {
	m.ackAt(reqStart, time.Now(), n)
}

// ackAt is ack with the acknowledgement time supplied, so that a request's
// position on the wall clock can be set exactly.
func (m *uploadAckMeter) ackAt(reqStart, at time.Time, n int64) {
	if n <= 0 || reqStart.IsZero() {
		return
	}
	elapsed := at.Sub(reqStart)

	m.mu.Lock()
	defer m.mu.Unlock()

	// The round trip is wall clock the worker spent waiting rather than
	// sending. Subtracting more than the request took would invent throughput,
	// so an implausible latency is ignored instead.
	transfer := elapsed - m.latency
	half := m.latency / 2
	if transfer <= 0 {
		transfer = elapsed
		half = 0
	}
	seconds := transfer.Seconds()
	if seconds <= 0 {
		return
	}

	// The first byte reaches the server half a round trip after it was
	// written, and the acknowledgement leaves it half a round trip before it
	// arrives.
	m.acked = append(m.acked, ackSpan{
		from:      reqStart.Add(half),
		to:        at.Add(-half),
		n:         n,
		qualified: transfer >= minQualifiedTransfer,
	})

	m.lastRate = float64(n) / seconds
	next := int64(m.lastRate * targetRequestDuration.Seconds())
	// Bounded by this request's own size, not by the shared payload: every
	// worker's first request is acknowledged at about the same moment, and
	// bounding by the shared value let each of them multiply the last one's
	// growth.
	if capped := n * maxGrowthFactor; next > capped {
		next = capped
	}
	if next < minUploadPayload {
		next = minUploadPayload
	} else if next > maxUploadPayload {
		next = maxUploadPayload
	}
	m.payload = next
}

// abandon records a request that began writing at reqStart and was never
// acknowledged — cancelled at the end of the phase, or rejected. A request that
// never wrote carries no position and is ignored.
func (m *uploadAckMeter) abandon(reqStart time.Time) {
	m.abandonAt(reqStart, time.Now())
}

// abandonAt is abandon with the time the request was given up supplied.
func (m *uploadAckMeter) abandonAt(reqStart, at time.Time) {
	if reqStart.IsZero() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abandoned = append(m.abandoned, abandonedSpan{from: reqStart.Add(m.latency / 2), at: at})
}

// rate reports the acknowledged upload rate in bytes per second, or 0 when
// nothing was acknowledged.
//
// It is the bytes the server acknowledged inside a steady window, divided by
// the window's wall-clock length:
//
//   - The window opens where the first qualified request began. Before that
//     the requests are small and mostly round trips, and the link is still
//     ramping; counting those seconds read 0.83x in the lab.
//   - It closes where the first request that was never acknowledged began, or
//     at the last acknowledged byte when none was abandoned. Past an abandoned
//     request some traffic is unknown; up to it everything is, including the
//     silence after a connection's last acknowledgement, which is part of what
//     the test delivered.
//   - A request that overran — in flight overrunFactor times longer than a
//     request is sized to take — does not close the window. Closing it there
//     would throw away the whole phase over one connection; leaving its
//     unknown bytes out costs at most that connection's share, and reads low.
//
// Every acknowledged request counts, qualified or not, with its bytes spread
// evenly over the interval the server spent receiving it, so one cut by an
// edge of the window counts only the share that fell inside. When no request
// qualified, the window opens at the first acknowledged byte instead.
func (m *uploadAckMeter) rate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.acked) == 0 {
		return 0
	}
	first, last := m.acked[0].from, m.acked[0].to
	var firstQualified time.Time
	for _, span := range m.acked {
		if span.from.Before(first) {
			first = span.from
		}
		if span.to.After(last) {
			last = span.to
		}
		if span.qualified && (firstQualified.IsZero() || span.from.Before(firstQualified)) {
			firstQualified = span.from
		}
	}

	end := last
	overrun := overrunFactor * (targetRequestDuration + m.latency)
	closed := false
	for _, a := range m.abandoned {
		if a.at.Sub(a.from) > overrun {
			continue
		}
		if !closed || a.from.Before(end) {
			end = a.from
			closed = true
		}
	}

	start := first
	if !firstQualified.IsZero() {
		start = firstQualified
	}
	if !end.After(start) {
		start, end = first, last
	}
	if !end.After(start) {
		return 0
	}

	var received float64
	for _, span := range m.acked {
		received += span.share(start, end)
	}
	return received / end.Sub(start).Seconds()
}

// share reports how many of the span's bytes the server received inside
// [start, end], assuming they arrived at an even rate.
func (s ackSpan) share(start, end time.Time) float64 {
	length := s.to.Sub(s.from)
	if length <= 0 {
		if !s.from.Before(start) && !s.from.After(end) {
			return float64(s.n)
		}
		return 0
	}
	from, to := s.from, s.to
	if from.Before(start) {
		from = start
	}
	if to.After(end) {
		to = end
	}
	if !to.After(from) {
		return 0
	}
	return float64(s.n) * to.Sub(from).Seconds() / length.Seconds()
}
