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
	ErrConnectTimeout = errors.New("server connect timeout")
)

func (s *Server) MultiDownloadTestContext(ctx context.Context, servers Servers) error {
	ss := servers.Available()
	if ss.Len() == 0 {
		return errors.New("not found available servers")
	}
	mainIDIndex := 0
	var td *TestDirection
	_context, cancel := context.WithCancel(ctx)
	defer cancel()
	tally := &phaseTally{}
	for i, server := range *ss {
		if server.ID == s.ID {
			mainIDIndex = i
		}
		sp := server
		dbg.Printf("Register Download Handler: %s\n", sp.URL)
		td = server.Context.RegisterDownloadHandler(func() {
			tally.note(downloadRequest(_context, sp, 3))
		})
	}
	if td == nil {
		return ErrorUninitializedManager
	}
	// Every server in the list carries the client as its Context, so one
	// manager accounts for all of them.
	before := td.manager.GetTotalDownload()
	td.Start(_context, cancel, mainIDIndex) // block here
	s.DLSpeed = ByteRate(td.manager.GetEWMADownloadRate())
	if s.DLSpeed == 0 && tally.mostlyFailed() {
		s.DLSpeed = -1 // N/A
	}
	return phaseError(ctx, tally, td.manager.GetTotalDownload()-before)
}

func (s *Server) MultiUploadTestContext(ctx context.Context, servers Servers) error {
	ss := servers.Available()
	if ss.Len() == 0 {
		return errors.New("not found available servers")
	}
	mainIDIndex := 0
	var td *TestDirection
	_context, cancel := context.WithCancel(ctx)
	defer cancel()
	tally := &phaseTally{}
	for i, server := range *ss {
		if server.ID == s.ID {
			mainIDIndex = i
		}
		sp := server
		dbg.Printf("Register Upload Handler: %s\n", sp.URL)
		td = server.Context.RegisterUploadHandler(func() {
			tally.note(uploadRequest(_context, sp))
		})
	}
	if td == nil {
		return ErrorUninitializedManager
	}
	td.manager.SetUploadLatency(s.Latency)
	// Every server in the list carries the client as its Context, so one
	// manager accounts for all of them.
	before := td.manager.GetTotalUpload()
	td.Start(_context, cancel, mainIDIndex) // block here
	s.ULSpeed = ByteRate(td.manager.GetAckedUploadRate())
	if s.ULSpeed == 0 && tally.mostlyFailed() {
		s.ULSpeed = -1 // N/A
	}
	return phaseError(ctx, tally, td.manager.GetTotalUpload()-before)
}

// DownloadTest executes the test to measure download speed
func (s *Server) DownloadTest() error {
	return s.downloadTestContext(context.Background(), downloadRequest)
}

// DownloadTestContext executes the test to measure download speed, observing the given context.
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
	s.Context.RegisterDownloadHandler(func() {
		tally.note(downloadRequest(_context, s, 3))
	}).Start(_context, cancel, 0)
	duration := time.Since(start)
	s.DLSpeed = ByteRate(s.Context.GetEWMADownloadRate())
	if s.DLSpeed == 0 && tally.mostlyFailed() {
		s.DLSpeed = -1 // N/A
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
func (s *Server) UploadTestContext(ctx context.Context) error {
	return s.uploadTestContext(ctx, uploadRequest)
}

func (s *Server) uploadTestContext(ctx context.Context, uploadRequest uploadFunc) error {
	tally := &phaseTally{}
	start := time.Now()
	_context, cancel := context.WithCancel(ctx)
	s.Context.SetUploadLatency(s.Latency)
	// See downloadTestContext: the verdict is about this phase, not about
	// everything the manager has ever sent.
	before := s.Context.GetTotalUpload()
	s.Context.RegisterUploadHandler(func() {
		tally.note(uploadRequest(_context, s))
	}).Start(_context, cancel, 0)
	duration := time.Since(start)
	s.ULSpeed = ByteRate(s.Context.GetAckedUploadRate())
	if s.ULSpeed == 0 && tally.mostlyFailed() {
		s.ULSpeed = -1 // N/A
	}
	s.TestDuration.Upload = &duration
	s.testDurationTotalCount()
	return phaseError(ctx, tally, s.Context.GetTotalUpload()-before)
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
		t.lastErr.Store(phaseCause{err: err})
	}
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
// transferred is what decides it, not the request tally, and it must be what
// this phase moved rather than the manager's lifetime total. Closing a phase
// cancels whatever is still in flight, so on a link too slow to finish one
// chunk inside the capture window every single request ends cancelled — the
// former fixed 976 KiB upload needed about 81 KB/s to complete within 12
// seconds, and a 1.89 MiB download about 162 KB/s. Counting those
// cancellations as failures reported a working link as unreachable, which
// excluded exactly the low-bandwidth links this client exists to measure.
// Bytes on the wire prove the endpoint answered, whatever became of the
// requests carrying them.
func phaseError(ctx context.Context, tally *phaseTally, transferred int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if transferred > 0 {
		return nil
	}
	requests, failures := tally.counts()
	// Nothing usable happened: either no request was ever dispatched, or every
	// dispatched one failed.
	if requests == 0 || failures == requests {
		if cause := tally.cause(); cause != nil {
			// The sentinel stays matchable with errors.Is; the concrete
			// transport error is what says whether to check the address, the
			// certificate or the link.
			return fmt.Errorf("%w: %v", ErrConnectTimeout, cause)
		}
		return ErrConnectTimeout
	}
	return nil
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
		return err
	}
	defer resp.Body.Close()
	// Only a success status proves the server read the body. An error status
	// can be written before the body has been read at all — a rejected size, a
	// refused credential — and acknowledging that would credit the link with
	// bytes it never carried, at whatever rate the rejection came back.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("upload rejected by %s: %s", s.URL, resp.Status)
	}
	// The response is sent only after the server has read the whole body, so
	// its arrival is what proves these bytes crossed the wire. A request cut
	// short by the capture window still ends with a well-formed chunked body
	// and is acknowledged for what it did send.
	s.Context.AckUpload(dc.WriteSpan())
	return nil
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
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
