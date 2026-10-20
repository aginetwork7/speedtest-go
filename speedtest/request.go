package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/showwin/speedtest-go/speedtest/transport"
)

type (
	downloadFunc func(context.Context, *Server, int) error
	uploadFunc   func(context.Context, *Server) error
)

var (
	dlSizes = [...]int{350, 500, 750, 1000, 1500, 2000, 2500, 3000, 3500, 4000}
)

var (
	// ErrConnectTimeout reports that nothing came back from the endpoint. The
	// name predates its current meaning and is kept so existing matches still
	// compile; it is not limited to timeouts.
	//
	// A throughput phase returns it when no request succeeded and nothing
	// proves the endpoint answered: a download received no bytes, an upload
	// had no request acknowledged. It then carries the last transport error in
	// its message — a DNS failure, a TLS error, a refused or reset connection,
	// a request still unanswered when the window closed — so read the message
	// to tell them apart; only ErrConnectTimeout itself matches errors.Is. It
	// is bare when no request was dispatched at all.
	//
	// TCPPing, HTTPPing and ICMPPing return it bare when every probe failed.
	ErrConnectTimeout = errors.New("server connect timeout")
	// ErrRequestRejected reports that the server answered with an error status.
	// It is distinct from a connection failure because the remedy is: a
	// rejected request points at the credential or the endpoint, not the link.
	ErrRequestRejected = errors.New("request rejected by server")
)

// checkStatus returns ErrRequestRejected, wrapped with what was asked of whom,
// unless resp carries a success status.
//
// Every request a throughput measurement rests on goes through here. An error
// status can be written before the request has been served at all — a refused
// credential is answered without reading the upload body or sending the
// download one — so treating it as an answer credits the link with traffic it
// never carried.
//
// The latency probe deliberately does not. It measures the round trip, and an
// error status makes the same round trip as a success; the deployment in fact
// serves no latency.txt and answers every probe 404. Requiring a success
// there failed every real run at its first phase.
func checkStatus(resp *http.Response, what string) error {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	return fmt.Errorf("%w: %s %s: %s", ErrRequestRejected, what, resp.Request.URL, resp.Status)
}

// MultiDownloadTestContext measures download speed across servers, which must
// share s's client, and leaves the rate in s.DLSpeed.
//
// It returns what DownloadTestContext does, and also fails before measuring when
// servers has none available.
func (s *Server) MultiDownloadTestContext(ctx context.Context, servers Servers) error {
	ss := servers.Available()
	if ss.Len() == 0 {
		return errors.New("not found available servers")
	}
	mainIDIndex := 0
	_context, cancel := context.WithCancel(ctx)
	defer cancel()
	tally := &phaseTally{}
	fns := make([]func(), 0, ss.Len())
	for i, server := range *ss {
		if server.ID == s.ID {
			mainIDIndex = i
		}
		sp := server
		dbg.Printf("Download Handler: %s\n", sp.URL)
		fns = append(fns, func() {
			tally.note(downloadRequest(_context, sp, 3))
		})
	}
	// Every server in the list carries the client as its Context, so one
	// manager accounts for all of them.
	manager := (*ss)[0].Context.Manager
	before := manager.GetTotalDownload()
	if _, err := runPhase(manager, downloadPhase, _context, cancel, mainIDIndex, fns); err != nil { // block here
		return err
	}
	s.DLSpeed = ByteRate(manager.GetEWMADownloadRate())
	if s.DLSpeed == 0 && tally.mostlyFailed() {
		s.DLSpeed = RateUnavailable
	}
	return phaseError(ctx, tally, manager.GetTotalDownload()-before)
}

// MultiUploadTestContext measures upload speed across servers, which must
// share s's client, and leaves the rate in s.ULSpeed.
//
// It returns what UploadTestContext does, and also fails before measuring when
// servers has none available.
func (s *Server) MultiUploadTestContext(ctx context.Context, servers Servers) error {
	ss := servers.Available()
	if ss.Len() == 0 {
		return errors.New("not found available servers")
	}
	mainIDIndex := 0
	_context, cancel := context.WithCancel(ctx)
	defer cancel()
	requestCtx, stopRequests := uploadRequestContext(ctx, _context, uploadPhaseGrace(s))
	defer stopRequests()
	tally := &phaseTally{}
	fns := make([]func(), 0, ss.Len())
	for i, server := range *ss {
		if server.ID == s.ID {
			mainIDIndex = i
		}
		sp := server
		dbg.Printf("Upload Handler: %s\n", sp.URL)
		fns = append(fns, func() {
			tally.note(uploadRequest(requestCtx, sp))
		})
	}
	// Every server in the list carries the client as its Context, so one
	// manager accounts for all of them.
	manager := (*ss)[0].Context.Manager
	manager.SetUploadLatency(s.Latency)
	before := manager.GetTotalUpload()
	td, err := runPhase(manager, uploadPhase, _context, cancel, mainIDIndex, fns) // block here
	if err != nil {
		return err
	}
	s.ULSpeed = ByteRate(manager.GetAckedUploadRate())
	if s.ULSpeed == 0 && tally.mostlyFailed() {
		s.ULSpeed = RateUnavailable
	}
	return uploadVerdict(ctx, tally, s.ULSpeed, uploadEvidence(td, manager.GetTotalUpload()-before))
}

// DownloadTest executes the test to measure download speed
func (s *Server) DownloadTest() error {
	return s.downloadTestContext(context.Background(), downloadRequest)
}

// DownloadTestContext executes the test to measure download speed, observing the given context.
//
// The returned error says whether the endpoint answered; the rate left in
// s.DLSpeed says whether anything measurable came back. The first of these
// that applies decides it:
//
//  1. The caller's ctx ended: ctx.Err(), even if bytes had arrived.
//  2. Some bytes arrived: nil.
//  3. No request succeeded: ErrRequestRejected (wrapped) if the last failure
//     was a refusal, otherwise ErrConnectTimeout.
//  4. Otherwise nil.
//
// Before any of these it returns ErrorUninitializedManager if the client's
// Manager is not the stock one and offers no direction to run on.
func (s *Server) DownloadTestContext(ctx context.Context) error {
	return s.downloadTestContext(ctx, downloadRequest)
}

func (s *Server) downloadTestContext(ctx context.Context, downloadRequest downloadFunc) error {
	tally := &phaseTally{}
	start := time.Now()
	_context, cancel := context.WithCancel(ctx)
	// What this phase moved, not what the manager has moved since it was
	// built: the byte counters run for the life of the manager, so a phase
	// reading them raw would inherit an earlier phase's bytes and call itself
	// successful on them.
	before := s.Context.GetTotalDownload()
	if _, err := runPhase(s.Context.Manager, downloadPhase, _context, cancel, 0, []func(){func() {
		tally.note(downloadRequest(_context, s, 3))
	}}); err != nil {
		cancel()
		return err
	}
	duration := time.Since(start)
	s.DLSpeed = ByteRate(s.Context.GetEWMADownloadRate())
	if s.DLSpeed == 0 && tally.mostlyFailed() {
		s.DLSpeed = RateUnavailable
	}
	s.TestDuration.Download = &duration
	s.testDurationTotalCount()
	return phaseError(ctx, tally, s.Context.GetTotalDownload()-before)
}

// UploadTest executes the test to measure upload speed
func (s *Server) UploadTest() error {
	return s.uploadTestContext(context.Background(), uploadRequest)
}

// UploadTestContext executes the test to measure upload speed, observing the given context.
//
// The returned error says whether the endpoint answered; the rate left in
// s.ULSpeed says whether anything measurable came back. The first of these
// that applies decides it:
//
//  1. The caller's ctx ended: ctx.Err(), even if requests were acknowledged.
//  2. No rate came out and the last failure was a refusal: ErrRequestRejected
//     (wrapped), even if some requests were acknowledged.
//  3. Some request was acknowledged: nil.
//  4. No request succeeded: ErrRequestRejected (wrapped) if the last failure
//     was a refusal, otherwise ErrConnectTimeout.
//  5. Otherwise nil.
//
// Step 2 is the one way this differs from DownloadTestContext. Bytes sent prove
// nothing, so an upload's evidence is acknowledgements, and a phase that ends
// on refusals with no rate is reported as refused. ErrorUninitializedManager
// comes first, as in DownloadTestContext.
func (s *Server) UploadTestContext(ctx context.Context) error {
	return s.uploadTestContext(ctx, uploadRequest)
}

func (s *Server) uploadTestContext(ctx context.Context, uploadRequest uploadFunc) error {
	tally := &phaseTally{}
	start := time.Now()
	_context, cancel := context.WithCancel(ctx)
	requestCtx, stopRequests := uploadRequestContext(ctx, _context, uploadPhaseGrace(s))
	defer stopRequests()
	s.Context.SetUploadLatency(s.Latency)
	// See downloadTestContext: the verdict is about this phase, not about
	// everything the manager has ever sent.
	before := s.Context.GetTotalUpload()
	td, err := runPhase(s.Context.Manager, uploadPhase, _context, cancel, 0, []func(){func() {
		tally.note(uploadRequest(requestCtx, s))
	}})
	if err != nil {
		cancel()
		return err
	}
	duration := time.Since(start)
	s.ULSpeed = ByteRate(s.Context.GetAckedUploadRate())
	if s.ULSpeed == 0 && tally.mostlyFailed() {
		s.ULSpeed = RateUnavailable
	}
	s.TestDuration.Upload = &duration
	s.testDurationTotalCount()
	return uploadVerdict(ctx, tally, s.ULSpeed, uploadEvidence(td, s.Context.GetTotalUpload()-before))
}

// phaseDirection names the direction of a manager that a built-in phase runs
// on, in the two forms runPhase can reach it by.
type phaseDirection struct {
	own      func(*DataManager) *TestDirection
	register func(Manager, func()) *TestDirection
}

var (
	downloadPhase = phaseDirection{
		own:      func(dm *DataManager) *TestDirection { return dm.download },
		register: Manager.RegisterDownloadHandler,
	}
	uploadPhase = phaseDirection{
		own:      func(dm *DataManager) *TestDirection { return dm.upload },
		register: Manager.RegisterUploadHandler,
	}
)

// runPhase runs a built-in phase's handlers on m's direction and returns that
// direction once every worker has stopped.
//
// With the stock DataManager the handlers go straight to the direction and
// never enter the list RegisterDownloadHandler/RegisterUploadHandler fill. That
// list belongs to callers of the exported API. A phase that shared it ran a
// caller's handlers beside its own, mixing their traffic into its reading; was
// crowded out when the list was already full, then reported a verdict and a
// rate for requests it never made; and cleared the caller's handlers on its way
// out, so their next Start panicked. At most nThread handlers run, as
// registration allowed.
//
// Any other Manager offers its directions only through registration, so the
// handlers go through the shared list as they always have, and are removed
// afterwards: they close over this run's context, which is cancelled once it
// ends. Mixing caller handlers with the package's phases stays unsound there.
func runPhase(m Manager, dir phaseDirection, ctx context.Context, cancel context.CancelFunc, mainIndex int, fns []func()) (*TestDirection, error) {
	if dm, ok := m.(*DataManager); ok {
		if len(fns) > dm.nThread {
			fns = fns[:dm.nThread]
		}
		td := dir.own(dm)
		td.run(ctx, cancel, mainIndex, fns)
		return td, nil
	}
	var td *TestDirection
	for _, fn := range fns {
		td = dir.register(m, fn)
	}
	if td == nil {
		return nil, ErrorUninitializedManager
	}
	td.Start(ctx, cancel, mainIndex)
	td.fns = nil
	return td, nil
}

// phaseTally records what one throughput phase attempted, and keeps the last
// error a request reported.
//
// The counters alone cannot say why a phase moved nothing: a refused
// connection, an expired certificate, a rejected request and a name that does
// not resolve all increment the same integer. Keeping the error costs one
// store per failed request and is the only thing that tells an operator which
// of those happened.
type phaseTally struct {
	requests int64
	failures int64

	lastErr atomic.Value // always a phaseCause
}

// phaseCause keeps the concrete type stored in lastErr constant. atomic.Value
// panics when successive stores disagree on type, and the errors arriving here
// are whatever the transport produced.
type phaseCause struct{ err error }

// note records the outcome of one request.
func (t *phaseTally) note(err error) {
	atomic.AddInt64(&t.requests, 1)
	if err != nil {
		atomic.AddInt64(&t.failures, 1)
		// Closing a phase cancels whatever is still in flight, so a
		// cancellation is nearly always the last error to arrive. Letting it
		// replace the one before would report every failed phase as cancelled
		// and hide what actually went wrong — a refused credential, an
		// unreachable host.
		if isCancellation(err) && t.lastErr.Load() != nil {
			return
		}
		t.lastErr.Store(phaseCause{err: err})
	}
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (t *phaseTally) counts() (requests, failures int64) {
	return atomic.LoadInt64(&t.requests), atomic.LoadInt64(&t.failures)
}

// mostlyFailed reports whether failures crossed the share that has always
// driven the -1 "N/A" rate sentinel.
func (t *phaseTally) mostlyFailed() bool {
	requests, failures := t.counts()
	if requests == 0 {
		return false
	}
	return float64(failures)/float64(requests) > 0.1
}

// noneSucceeded reports the two ways a phase can end with nothing usable:
// no request was ever dispatched, and every dispatched request failed. They
// are one predicate because the verdict treats them alike, and separate
// clauses because neither implies the other.
func (t *phaseTally) noneSucceeded() bool {
	requests, failures := t.counts()
	return requests == 0 || failures == requests
}

// cause reports the last error a request produced, or nil if none did.
func (t *phaseTally) cause() error {
	if boxed, ok := t.lastErr.Load().(phaseCause); ok {
		return boxed.err
	}
	return nil
}

// phaseError reports whether a throughput phase produced anything usable.
//
// ctx is the caller's context rather than the derived one, which is always
// cancelled on the way out as part of closing the phase.
//
// Without this verdict a phase reports success no matter what happened, so a
// caller cannot distinguish an unreachable server from a genuinely idle link:
// both arrive as a rate of zero and a nil error.
//
// evidence is what decides it, not the request tally: how much of what this
// phase did proves the endpoint answered. It must count this phase only, not
// the manager's lifetime. Closing a phase cancels whatever is still in flight,
// so on a link too slow to finish one chunk inside the capture window every
// single request ends cancelled — a 1.89 MiB download needs about 162 KB/s to
// complete within 12 seconds. Counting those cancellations as failures reported
// a working link as unreachable, which excluded exactly the low-bandwidth links
// this client exists to measure.
//
// A download passes the bytes it received: they came from the endpoint,
// whatever became of the requests carrying them. An upload cannot pass the
// bytes it sent, which are counted when handed to the socket; see uploadVerdict.
func phaseError(ctx context.Context, tally *phaseTally, evidence int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if evidence > 0 {
		return nil
	}
	if tally.noneSucceeded() {
		cause := tally.cause()
		// A rejection is an answer, so calling it a connect timeout would send
		// the operator to check the link instead of the credential.
		if errors.Is(cause, ErrRequestRejected) {
			return cause
		}
		if cause != nil {
			// The sentinel stays matchable with errors.Is; the concrete
			// transport error is what says whether to check the address, the
			// certificate or the link.
			return fmt.Errorf("%w: %v", ErrConnectTimeout, cause)
		}
		return ErrConnectTimeout
	}
	return nil
}

// uploadAckGraceBase, uploadAckGraceRoundTrips and uploadAckGraceWindowShare
// bound how long an upload phase waits, after its capture window closes, for
// requests already in flight to be acknowledged.
const (
	uploadAckGraceBase        = time.Second
	uploadAckGraceRoundTrips  = 2
	uploadAckGraceWindowShare = 4 // a quarter of the capture window
)

// uploadAckGrace is the larger of an acknowledgement's round trips plus a
// second, and a quarter of the capture window. The round trips cover a request
// whose last bytes are just arriving; the share of the window covers one slowed
// by loss, which in the lab finished a second past the round-trip bound and was
// thrown away with everything it carried.
func uploadAckGrace(latency, capture time.Duration) time.Duration {
	grace := uploadAckGraceBase + uploadAckGraceRoundTrips*latency
	if share := capture / uploadAckGraceWindowShare; share > grace {
		grace = share
	}
	return grace
}

// uploadPhaseGrace is the grace an upload phase on s waits for in-flight
// acknowledgements; both upload entry points take it from here.
func uploadPhaseGrace(s *Server) time.Duration {
	return uploadAckGrace(s.Latency, captureTimeOf(s))
}

// captureTimeOf reports the capture window of the manager behind s, or 0 when
// it is not the stock DataManager.
func captureTimeOf(s *Server) time.Duration {
	if dm, ok := s.Context.Manager.(*DataManager); ok {
		return dm.captureTime
	}
	return 0
}

// uploadRequestContext returns the context upload requests run under, and a
// function that releases it.
//
// It outlives the phase context by a grace period. When the capture window
// closes the writers stop and no new request starts, but a request in flight
// has usually been read by the server already, with only its acknowledgement
// on the way back. Cancelling it with the phase threw those bytes away: the
// rate window then had to close before them, and on a lossy single connection
// in the lab that left 1.1 s of a 10 s phase to measure, read 3x high. The
// grace is set by uploadAckGrace, and it is bounded so that a link that has
// stalled cannot hold the phase open.
//
// A cancellation from the caller is not a window closing on schedule: the
// request context derives from the caller's, so it stops requests at once
// without waiting out the grace.
func uploadRequestContext(caller, phase context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	requestCtx, cancel := context.WithCancel(caller)
	go func() {
		select {
		case <-requestCtx.Done():
			return
		case <-phase.Done():
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-requestCtx.Done():
		}
	}()
	return requestCtx, cancel
}

// uploadVerdict is phaseError for an upload phase, whose evidence is the
// requests the server acknowledged rather than the bytes the phase sent.
//
// Upload bytes are counted when they are handed to the socket, so they prove
// nothing reached the endpoint: a body fits in the kernel's send buffer whether
// the server is reading it, has stopped answering, or is about to reset the
// connection. Passing a phase on them returned nil from a server that
// acknowledged nothing, beside a rate of RateUnavailable, which is the broken
// endpoint reading as an idle one. Only an acknowledgement shows the server
// received a request.
//
// The cost is the link too slow to finish one request inside the window: its
// phase now fails instead of passing. That link yields no upload rate either
// way, since the rate is built from acknowledgements, so no measurement is lost.
//
// When nothing measurable came back and a refusal is what the phase ended on,
// the refusal is the verdict even if some requests were acknowledged —
// reporting only that no rate came out sent operators to the link while the
// server had no upload route.
func uploadVerdict(ctx context.Context, tally *phaseTally, rate ByteRate, acknowledged int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rate <= 0 {
		if cause := tally.cause(); errors.Is(cause, ErrRequestRejected) {
			return cause
		}
	}
	return phaseError(ctx, tally, acknowledged)
}

// uploadEvidence reports how many requests of the phase that just ran on td
// the server acknowledged. A direction without the stock acknowledgement meter
// can only offer the bytes it sent, which is what the verdict used before.
func uploadEvidence(td *TestDirection, sent int64) int64 {
	if td.ackMeter == nil {
		return sent
	}
	return int64(td.ackMeter.acknowledged())
}

func downloadRequest(ctx context.Context, s *Server, w int) error {
	size := dlSizes[w]
	u, err := url.Parse(s.URL)
	if err != nil {
		return err
	}
	u.Path = path.Dir(u.Path)
	xdlURL := u.JoinPath(fmt.Sprintf("random%dx%d.jpg", size, size)).String()
	dbg.Printf("XdlURL: %s\n", xdlURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xdlURL, nil)
	if err != nil {
		return err
	}
	if len(s.Context.config.Credential) > 0 {
		req.Header.Set("Authorization", s.Context.config.Credential)
	}

	resp, err := s.Context.doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, "download"); err != nil {
		// The error body is not the payload; counting it would report the
		// rejection's own bytes as download throughput.
		_, _ = io.Copy(io.Discard, resp.Body)
		return err
	}
	return s.Context.NewChunk().DownloadHandler(resp.Body)
}

// uploadRequest sends one upload request and, if the server answers, reports
// the bytes it acknowledged.
//
// It takes no size index: the body size comes from the acknowledgement meter,
// which sizes each request from what the previous one achieved. The former
// fixed 976 KiB body takes 80 seconds on a 100 kbps link, so no request would
// be acknowledged inside a capture window at all — on exactly the links this
// client exists to measure.
func uploadRequest(ctx context.Context, s *Server) error {
	dc := s.Context.NewChunk().UploadHandler(s.Context.NextUploadPayload())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, io.NopCloser(dc))
	if err != nil {
		return err
	}
	dbg.Printf("Len=%d, XulURL: %s\n", req.ContentLength, s.URL)
	req.Header.Set("Content-Type", "application/octet-stream")
	if len(s.Context.config.Credential) > 0 {
		req.Header.Set("Authorization", s.Context.config.Credential)
	}
	resp, err := s.Context.doer.Do(req)
	if err != nil {
		// Cancelled at the end of the phase, most often. Whatever it wrote may
		// or may not have arrived, so the rate must not be measured past it.
		writeStart, _ := dc.WriteSpan()
		s.Context.AbandonUpload(writeStart)
		return err
	}
	defer drainAndClose(resp.Body)
	// Only a success status proves the server read the body. An error status
	// can be written before the body has been read at all — a rejected size, a
	// refused credential — and acknowledging that would credit the link with
	// bytes it never carried, at whatever rate the rejection came back.
	if err := checkStatus(resp, "upload"); err != nil {
		writeStart, _ := dc.WriteSpan()
		s.Context.AbandonUpload(writeStart)
		return err
	}
	// The response is sent only after the server has read the whole body, so
	// its arrival is what proves these bytes crossed the wire. A request cut
	// short by the capture window still ends with a well-formed chunked body
	// and is acknowledged for what it did send.
	s.Context.AckUpload(dc.WriteSpan())
	return nil
}

// maxUploadReplyDrain bounds how much of an upload reply is read before the
// response is closed. The upload endpoint answers with a few bytes ("size=N");
// a reply larger than this is not worth keeping the connection for.
const maxUploadReplyDrain = 64 << 10

// drainAndClose reads an upload reply to its end before closing it. net/http
// returns a connection to the pool only when its response body reached EOF;
// closing one unread discards the connection, so every request paid a fresh
// handshake and restarted slow start. With one connection at a 300 ms round
// trip, charging each new connection one round trip, that read 0.22 of the
// link against 0.63 with the connection reused.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, maxUploadReplyDrain)
	_ = body.Close()
}

// PingTest executes test to measure latency
func (s *Server) PingTest(callback func(latency time.Duration)) error {
	return s.PingTestContext(context.Background(), callback)
}

// PingTestContext executes test to measure latency, observing the given context.
func (s *Server) PingTestContext(ctx context.Context, callback func(latency time.Duration)) (err error) {
	start := time.Now()
	var vectorPingResult []int64
	if s.Context.config.PingMode == TCP {
		vectorPingResult, err = s.TCPPing(ctx, 10, time.Millisecond*200, callback)
	} else if s.Context.config.PingMode == ICMP {
		vectorPingResult, err = s.ICMPPing(ctx, time.Second*4, 10, time.Millisecond*200, callback)
	} else {
		vectorPingResult, err = s.HTTPPing(ctx, 10, time.Millisecond*200, callback)
	}
	if err != nil || len(vectorPingResult) == 0 {
		return err
	}
	dbg.Printf("Before StandardDeviation: %v\n", vectorPingResult)
	mean, _, std, minLatency, maxLatency := StandardDeviation(vectorPingResult)
	duration := time.Since(start)
	s.Latency = time.Duration(mean) * time.Nanosecond
	s.Jitter = time.Duration(std) * time.Nanosecond
	s.MinLatency = time.Duration(minLatency) * time.Nanosecond
	s.MaxLatency = time.Duration(maxLatency) * time.Nanosecond
	s.TestDuration.Ping = &duration
	s.testDurationTotalCount()
	return nil
}

// TestAll executes ping, download and upload tests one by one
func (s *Server) TestAll() error {
	err := s.PingTest(nil)
	if err != nil {
		return err
	}
	err = s.DownloadTest()
	if err != nil {
		return err
	}
	return s.UploadTest()
}

func (s *Server) TCPPing(
	ctx context.Context,
	echoTimes int,
	echoFreq time.Duration,
	callback func(latency time.Duration),
) (latencies []int64, err error) {
	var pingDst string
	if len(s.Host) == 0 {
		u, err := url.Parse(s.URL)
		if err != nil || len(u.Host) == 0 {
			return nil, err
		}
		pingDst = u.Host
	} else {
		pingDst = s.Host
	}
	failTimes := 0
	client, err := transport.NewClient(s.Context.tcpDialer)
	if err != nil {
		return nil, err
	}
	err = client.Connect(ctx, pingDst)
	if err != nil {
		return nil, err
	}
	for i := 0; i < echoTimes; i++ {
		latency, err := client.PingContext(ctx)
		if err != nil {
			failTimes++
			continue
		}
		latencies = append(latencies, latency)
		if callback != nil {
			callback(time.Duration(latency))
		}
		time.Sleep(echoFreq)
	}
	if failTimes == echoTimes {
		return nil, ErrConnectTimeout
	}
	return
}

func (s *Server) HTTPPing(
	ctx context.Context,
	echoTimes int,
	echoFreq time.Duration,
	callback func(latency time.Duration),
) (latencies []int64, err error) {
	var contextErr error
	u, err := url.Parse(s.URL)
	if err != nil || len(u.Host) == 0 {
		return nil, err
	}
	u.Path = path.Dir(u.Path)
	pingDst := u.JoinPath("latency.txt").String()
	dbg.Printf("Echo: %s\n", pingDst)
	failTimes := 0
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pingDst, nil)
	if err != nil {
		return nil, err
	}
	if len(s.Context.config.Credential) > 0 {
		req.Header.Set("Authorization", s.Context.config.Credential)
	}
	// carry out an extra request to warm up the connection and ensure the first request is not going to affect the
	// overall estimation
	echoTimes++
	for i := 0; i < echoTimes; i++ {
		sTime := time.Now()
		resp, err := s.Context.doer.Do(req)
		endTime := time.Since(sTime)
		if err != nil {
			if isCancellation(err) {
				contextErr = err
				break
			}

			failTimes++
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if i > 0 {
			latency := endTime.Nanoseconds()
			latencies = append(latencies, latency)
			dbg.Printf("RTT: %d\n", latency)
			if callback != nil {
				callback(endTime)
			}
		}
		time.Sleep(echoFreq)
	}

	if contextErr != nil {
		return latencies, contextErr
	}

	if failTimes == echoTimes {
		return nil, ErrConnectTimeout
	}

	return
}

const PingTimeout = -1
const echoOptionDataSize = 32 // `echoMessage` need to change at same time

// ICMPPing privileged method
func (s *Server) ICMPPing(
	ctx context.Context,
	readTimeout time.Duration,
	echoTimes int,
	echoFreq time.Duration,
	callback func(latency time.Duration),
) (latencies []int64, err error) {
	u, err := url.ParseRequestURI(s.URL)
	if err != nil || len(u.Host) == 0 {
		return nil, err
	}
	dbg.Printf("Echo: %s\n", strings.Split(u.Host, ":")[0])
	dialContext, err := s.Context.ipDialer.DialContext(ctx, "ip:icmp", strings.Split(u.Host, ":")[0])
	if err != nil {
		return nil, err
	}
	defer dialContext.Close()

	ICMPData := make([]byte, 8+echoOptionDataSize) // header + data
	ICMPData[0] = 8                                // echo
	ICMPData[1] = 0                                // code
	ICMPData[2] = 0                                // checksum
	ICMPData[3] = 0                                // checksum
	ICMPData[4] = 0                                // id
	ICMPData[5] = 1                                // id
	ICMPData[6] = 0                                // seq
	ICMPData[7] = 1                                // seq

	var echoMessage = "Hi! SpeedTest-Go \\(●'◡'●)/"

	for i := 0; i < len(echoMessage); i++ {
		ICMPData[7+i] = echoMessage[i]
	}

	failTimes := 0
	for i := 0; i < echoTimes; i++ {
		ICMPData[2] = byte(0)
		ICMPData[3] = byte(0)

		ICMPData[6] = byte(1 >> 8)
		ICMPData[7] = byte(1)
		ICMPData[8+echoOptionDataSize-1] = 6
		cs := checkSum(ICMPData)
		ICMPData[2] = byte(cs >> 8)
		ICMPData[3] = byte(cs)

		sTime := time.Now()
		_ = dialContext.SetDeadline(sTime.Add(readTimeout))
		_, err = dialContext.Write(ICMPData)
		if err != nil {
			failTimes += echoTimes - i
			break
		}
		buf := make([]byte, 20+echoOptionDataSize+8)
		_, err = dialContext.Read(buf)
		if err != nil || buf[20] != 0x00 {
			failTimes++
			continue
		}
		endTime := time.Since(sTime)
		latencies = append(latencies, endTime.Nanoseconds())
		dbg.Printf("1RTT: %s\n", endTime)
		if callback != nil {
			callback(endTime)
		}
		time.Sleep(echoFreq)
	}
	if failTimes == echoTimes {
		return nil, ErrConnectTimeout
	}
	return
}

func checkSum(data []byte) uint16 {
	var sum uint32
	var length = len(data)
	var index int
	for length > 1 {
		sum += uint32(data[index])<<8 + uint32(data[index+1])
		index += 2
		length -= 2
	}
	if length > 0 {
		sum += uint32(data[index])
	}
	sum += sum >> 16
	return uint16(^sum)
}

func StandardDeviation(vector []int64) (mean, variance, stdDev, min, max int64) {
	if len(vector) == 0 {
		return
	}
	var sumNum, accumulate int64
	min = math.MaxInt64
	max = math.MinInt64
	for _, value := range vector {
		sumNum += value
		if min > value {
			min = value
		}
		if max < value {
			max = value
		}
	}
	mean = sumNum / int64(len(vector))
	for _, value := range vector {
		accumulate += (value - mean) * (value - mean)
	}
	variance = accumulate / int64(len(vector))
	stdDev = int64(math.Sqrt(float64(variance)))
	return
}
