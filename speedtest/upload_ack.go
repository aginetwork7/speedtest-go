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
// Two corrections make that signal usable, both found on a real 800 kbps path
// where the uncorrected version read 19% low with occasional readings at more
// than twice the link rate:
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
)

// uploadAckMeter sizes upload requests and derives the rate from the ones the
// server acknowledged. It is created per phase and is safe for concurrent use
// by the phase's workers.
type uploadAckMeter struct {
	mu sync.Mutex

	payload  int64         // body size for the next request
	latency  time.Duration // measured round trip, excluded from transfer time
	lastRate float64       // bytes/sec from the most recent acknowledgement
	deadline time.Time     // when the capture window closes

	// qualified holds requests that ran long enough to measure the link.
	qualBytes   int64
	qualSeconds float64

	// all holds every acknowledged request, as a fallback for a phase that
	// ended before any request qualified. Better a reading drawn from short
	// requests than no reading at all.
	allBytes   int64
	allSeconds float64
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
	if n <= 0 || reqStart.IsZero() {
		return
	}
	elapsed := time.Since(reqStart)

	m.mu.Lock()
	defer m.mu.Unlock()

	// The round trip is wall clock the worker spent waiting rather than
	// sending. Subtracting more than the request took would invent throughput,
	// so an implausible latency is ignored instead.
	transfer := elapsed - m.latency
	if transfer <= 0 {
		transfer = elapsed
	}
	seconds := transfer.Seconds()
	if seconds <= 0 {
		return
	}

	m.allBytes += n
	m.allSeconds += seconds
	if transfer >= minQualifiedTransfer {
		m.qualBytes += n
		m.qualSeconds += seconds
	}

	m.lastRate = float64(n) / seconds
	next := int64(m.lastRate * targetRequestDuration.Seconds())
	if capped := m.payload * maxGrowthFactor; next > capped {
		next = capped
	}
	if next < minUploadPayload {
		next = minUploadPayload
	} else if next > maxUploadPayload {
		next = maxUploadPayload
	}
	m.payload = next
}

// rate reports the acknowledged upload rate in bytes per second, or 0 when
// nothing was acknowledged.
//
// workers is how many requests ran concurrently. Their transfer times overlap
// in wall clock, so the summed transfer time has to be divided back down to the
// wall time the link was actually busy.
func (m *uploadAckMeter) rate(workers int) float64 {
	if workers < 1 {
		workers = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.qualSeconds > 0 {
		return float64(m.qualBytes) / (m.qualSeconds / float64(workers))
	}
	if m.allSeconds > 0 {
		return float64(m.allBytes) / (m.allSeconds / float64(workers))
	}
	return 0
}
