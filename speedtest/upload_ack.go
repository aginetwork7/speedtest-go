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
// product and the peer's receive window, together around a megabyte. That queue
// is a fixed size, so its share of the measurement grows as the link slows, and
// cutting a phase mid-request banks the whole of it as sent. Against an origin
// that counted the bytes it actually received, that read +92% on an 819 kbps
// link while the download in the same run read -1%.
//
// An HTTP response is the one per-request acknowledgement that cannot be
// inflated this way: the server sends it only after reading the whole body.
// Measuring acknowledged bytes over the time it took to have them acknowledged
// reproduced the wire rate exactly at 819 kbps and 3.3 Mbps.
const (
	// targetRequestDuration is how long one upload request should take. Long
	// enough that per-request overhead — connection setup, headers, the
	// server's response — stays a small share of it, short enough that a
	// capture window collects several acknowledgements.
	targetRequestDuration = 2 * time.Second

	// minUploadPayload bounds how slow a link can be and still have one request
	// acknowledged inside the shortest usable capture window: 16 KiB needs
	// about 13 kbps to complete in 10 seconds. It is also the size of the first
	// request, before any rate is known.
	minUploadPayload = 16 << 10

	// maxUploadPayload caps what a single request may cost on a fast link.
	// 128 MiB is about one second of gigabit.
	maxUploadPayload = 128 << 20

	// maxGrowthFactor bounds how far one request may resize the next. The first
	// request is deliberately small, so its duration is dominated by connection
	// setup and round trips rather than by bandwidth, and the rate derived from
	// it can be off by orders of magnitude. Unbounded, that one sample sizes a
	// request too large to finish inside the capture window — which leaves the
	// phase with nothing but the bad sample it started from.
	maxGrowthFactor = 8

	// completionSafetyFactor is the share of the remaining capture window a
	// request may be sized to occupy. A request that outlives the window is
	// cancelled before the server answers, so it is never acknowledged and its
	// bytes are measured by nothing.
	completionSafetyFactor = 0.5

	// settledGrowthRatio is the point at which the payload is considered sized
	// for the link. Below this, the request is still ramping and its duration
	// is dominated by round trips rather than by bandwidth, so counting it
	// would drag the reported rate down.
	settledGrowthRatio = 1.5
)

// uploadAckMeter sizes upload requests and derives the rate from the ones the
// server acknowledged. It is created per phase and is safe for concurrent use
// by the phase's workers.
type uploadAckMeter struct {
	mu sync.Mutex

	payload  int64     // body size for the next request
	settled  bool      // the payload has stopped ramping
	lastRate float64   // bytes/sec from the most recent acknowledgement
	deadline time.Time // when the capture window closes

	// all covers every acknowledged request in the phase. It is the fallback
	// for a phase that ended before the ramp settled, where discarding the ramp
	// would leave nothing at all.
	allStart, allLast time.Time
	allAcked          int64

	// steady covers only requests that began after the payload settled, which
	// is the window that reflects the link rather than the ramp.
	steadyStart, steadyLast time.Time
	steadyAcked             int64
}

func newUploadAckMeter() *uploadAckMeter {
	return &uploadAckMeter{payload: minUploadPayload}
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

// noteWrite records when the phase first put a byte on the wire. The rate is
// measured from here rather than from the phase's start so that it excludes
// the caller's own setup.
func (m *uploadAckMeter) noteWrite(at time.Time, window time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allStart.IsZero() {
		m.allStart = at
		m.deadline = at.Add(window)
	}
}

// ack records that the server acknowledged n bytes from a request that began
// writing at reqStart, and resizes the next request from what that one achieved.
func (m *uploadAckMeter) ack(reqStart time.Time, n int64) {
	if n <= 0 {
		return
	}
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.allStart.IsZero() || reqStart.Before(m.allStart) {
		m.allStart = reqStart
	}
	m.allAcked += n
	m.allLast = now

	// Requests already in flight when the window opened carry bytes from before
	// it and would inflate the steady rate, so only later ones count.
	if m.settled && !reqStart.Before(m.steadyStart) {
		m.steadyAcked += n
		m.steadyLast = now
	}

	elapsed := now.Sub(reqStart)
	if elapsed <= 0 {
		return
	}
	m.lastRate = float64(n) / elapsed.Seconds()
	next := int64(m.lastRate * targetRequestDuration.Seconds())
	if capped := m.payload * maxGrowthFactor; next > capped {
		next = capped
	}
	if next < minUploadPayload {
		next = minUploadPayload
	} else if next > maxUploadPayload {
		next = maxUploadPayload
	}
	if !m.settled && float64(next) <= float64(m.payload)*settledGrowthRatio {
		m.settled = true
		m.steadyStart = now
	}
	m.payload = next
}

// rate reports the acknowledged upload rate in bytes per second, or 0 when
// nothing was acknowledged.
func (m *uploadAckMeter) rate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.steadyAcked > 0 && m.steadyLast.After(m.steadyStart) {
		return float64(m.steadyAcked) / m.steadyLast.Sub(m.steadyStart).Seconds()
	}
	if m.allAcked > 0 && m.allLast.After(m.allStart) {
		return float64(m.allAcked) / m.allLast.Sub(m.allStart).Seconds()
	}
	return 0
}
