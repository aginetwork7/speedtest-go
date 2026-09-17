package speedtest

import (
	"bytes"
	"context"
	"errors"
	"github.com/showwin/speedtest-go/speedtest/internal"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Manager interface {
	SetRateCaptureFrequency(duration time.Duration) Manager
	SetCaptureTime(duration time.Duration) Manager

	NewChunk() Chunk

	GetTotalDownload() int64
	GetTotalUpload() int64
	AddTotalDownload(value int64)
	AddTotalUpload(value int64)

	GetAvgDownloadRate() float64
	GetAvgUploadRate() float64

	GetEWMADownloadRate() float64
	GetEWMAUploadRate() float64

	// Upload rate measured against server acknowledgements. See upload_ack.go
	// for why the byte counters above cannot answer this for uploads.
	NextUploadPayload() int64
	AckUpload(writeStart time.Time, written int64)
	GetAckedUploadRate() float64

	SetCallbackDownload(callback func(downRate ByteRate))
	SetCallbackUpload(callback func(upRate ByteRate))

	RegisterDownloadHandler(fn func()) *TestDirection
	RegisterUploadHandler(fn func()) *TestDirection

	// Wait for the upload or download task to end to avoid errors caused by core occupation
	Wait()
	Reset()
	Snapshots() *Snapshots

	SetNThread(n int) Manager
}

type Chunk interface {
	UploadHandler(size int64) Chunk
	DownloadHandler(r io.Reader) error

	GetRate() float64
	GetDuration() time.Duration
	GetParent() Manager

	// WriteSpan reports when this chunk put its first byte on the wire and how
	// many bytes it wrote, which is what the server acknowledges by responding.
	WriteSpan() (start time.Time, written int64)

	Read(b []byte) (n int, err error)
}

const readChunkSize = 1024 // 1 KBytes with higher frequency rate feedback

type DataType int32

const (
	typeEmptyChunk = iota
	typeDownload
	typeUpload
)

var (
	ErrorUninitializedManager = errors.New("uninitialized manager")
)

type funcGroup struct {
	fns []func()
}

func (f *funcGroup) Add(fn func()) {
	f.fns = append(f.fns, fn)
}

type DataManager struct {
	SnapshotStore *Snapshots
	Snapshot      *Snapshot
	sync.Mutex

	repeatByte *[]byte

	captureTime          time.Duration
	rateCaptureFrequency time.Duration
	nThread              int

	running   bool
	runningRW sync.RWMutex

	download *TestDirection
	upload   *TestDirection
}

type TestDirection struct {
	TestType        int                         // test type
	manager         *DataManager                // manager
	totalDataVolume int64                       // total send/receive data volume
	RateSequence    []int64                     // rate history sequence
	welford         *internal.Welford           // std/EWMA/mean
	captureCallback func(realTimeRate ByteRate) // user callback
	closeFunc       func()                      // close func
	ackMeter        *uploadAckMeter             // upload only, see upload_ack.go
	*funcGroup                                  // actually exec function
}

func (dm *DataManager) NewDataDirection(testType int) *TestDirection {
	td := &TestDirection{
		TestType:  testType,
		manager:   dm,
		funcGroup: &funcGroup{},
	}
	if testType == typeUpload {
		td.ackMeter = newUploadAckMeter()
	}
	return td
}

func NewDataManager() *DataManager {
	r := bytes.Repeat([]byte{0xAA}, readChunkSize) // uniformly distributed sequence of bits
	ret := &DataManager{
		nThread:              runtime.NumCPU(),
		captureTime:          time.Second * 15,
		rateCaptureFrequency: time.Millisecond * 50,
		Snapshot:             &Snapshot{},
		repeatByte:           &r,
	}
	ret.download = ret.NewDataDirection(typeDownload)
	ret.upload = ret.NewDataDirection(typeUpload)
	ret.SnapshotStore = newHistorySnapshots(maxSnapshotSize)
	return ret
}

func (dm *DataManager) SetCallbackDownload(callback func(downRate ByteRate)) {
	if dm.download != nil {
		dm.download.captureCallback = callback
	}
}

func (dm *DataManager) SetCallbackUpload(callback func(upRate ByteRate)) {
	if dm.upload != nil {
		dm.upload.captureCallback = callback
	}
}

func (dm *DataManager) Wait() {
	oldDownTotal := dm.GetTotalDownload()
	oldUpTotal := dm.GetTotalUpload()
	for {
		time.Sleep(dm.rateCaptureFrequency)
		newDownTotal := dm.GetTotalDownload()
		newUpTotal := dm.GetTotalUpload()
		deltaDown := newDownTotal - oldDownTotal
		deltaUp := newUpTotal - oldUpTotal
		oldDownTotal = newDownTotal
		oldUpTotal = newUpTotal
		if deltaDown == 0 && deltaUp == 0 {
			return
		}
	}
}

func (dm *DataManager) RegisterUploadHandler(fn func()) *TestDirection {
	if len(dm.upload.fns) < dm.nThread {
		dm.upload.Add(fn)
	}
	return dm.upload
}

func (dm *DataManager) RegisterDownloadHandler(fn func()) *TestDirection {
	if len(dm.download.fns) < dm.nThread {
		dm.download.Add(fn)
	}
	return dm.download
}

func (td *TestDirection) GetTotalDataVolume() int64 {
	return atomic.LoadInt64(&td.totalDataVolume)
}

func (td *TestDirection) AddTotalDataVolume(delta int64) int64 {
	return atomic.AddInt64(&td.totalDataVolume, delta)
}

// Start runs the registered handlers until the capture window closes or ctx
// ends, then blocks until every worker has stopped.
//
// ctx must be the same context the handlers issue their requests with: when it
// ends, the handlers fail immediately, so a worker that kept looping on the
// running flag alone would spin at full CPU until the capture timer fired.
func (td *TestDirection) Start(ctx context.Context, cancel context.CancelFunc, mainRequestHandlerIndex int) {
	if len(td.fns) == 0 {
		panic("empty task stack")
	}
	if mainRequestHandlerIndex > len(td.fns)-1 {
		mainRequestHandlerIndex = 0
	}
	mainLoadFactor := 0.1
	// When the number of processor cores is equivalent to the processing program,
	// the processing efficiency reaches the highest level (VT is not considered).
	mainN := int(mainLoadFactor * float64(len(td.fns)))
	if mainN == 0 {
		mainN = 1
	}
	if len(td.fns) == 1 {
		mainN = td.manager.nThread
	}
	auxN := td.manager.nThread - mainN
	dbg.Printf("Available fns: %d\n", len(td.fns))
	dbg.Printf("mainN: %d\n", mainN)
	dbg.Printf("auxN: %d\n", auxN)
	wg := sync.WaitGroup{}
	td.manager.running = true
	stopCapture := td.rateCapture()

	// refresh once function
	once := sync.Once{}
	td.closeFunc = func() {
		once.Do(func() {
			stopCapture <- true
			close(stopCapture)
			td.manager.runningRW.Lock()
			td.manager.running = false
			td.manager.runningRW.Unlock()
			cancel()
			dbg.Println("FuncGroup: Stop")
		})
	}

	// worker repeatedly invokes one handler until the direction is closed.
	// Keeping this in one place stops the two spawn loops below from drifting
	// apart, which would leave one of them ignoring cancellation.
	worker := func(fnIndex int) {
		defer wg.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			td.manager.runningRW.RLock()
			running := td.manager.running
			td.manager.runningRW.RUnlock()
			if !running {
				return
			}
			td.fns[fnIndex]()
		}
	}

	time.AfterFunc(td.manager.captureTime, td.closeFunc)
	for i := 0; i < mainN; i++ {
		wg.Add(1)
		go worker(mainRequestHandlerIndex)
	}
	for j := 0; j < auxN; {
		for i := range td.fns {
			if j == auxN {
				break
			}
			if i == mainRequestHandlerIndex {
				continue
			}
			wg.Add(1)
			go worker(i)
			j++
		}
	}
	wg.Wait()

	// Close the direction whatever stopped the workers. They also exit when the
	// context ends, and on that path nothing else would stop the rate capture
	// goroutine — leaving it writing the Welford state while the caller reads
	// the final rate out of it.
	td.closeFunc()
}

func (td *TestDirection) rateCapture() chan bool {
	ticker := time.NewTicker(td.manager.rateCaptureFrequency)
	var prevTotalDataVolume int64 = 0
	stopCapture := make(chan bool)
	td.welford = internal.NewWelford(5*time.Second, td.manager.rateCaptureFrequency)
	sTime := time.Now()
	go func(t *time.Ticker) {
		defer t.Stop()
		for {
			select {
			case <-t.C:
				newTotalDataVolume := td.GetTotalDataVolume()
				deltaDataVolume := newTotalDataVolume - prevTotalDataVolume
				prevTotalDataVolume = newTotalDataVolume
				if deltaDataVolume != 0 {
					td.RateSequence = append(td.RateSequence, deltaDataVolume)
				}
				// anyway we update the measuring instrument
				globalAvg := (float64(td.GetTotalDataVolume())) / float64(time.Since(sTime).Milliseconds()) * 1000
				if td.welford.Update(globalAvg, float64(deltaDataVolume)) {
					go td.closeFunc()
				}
				// reports the current rate at the given rate
				if td.captureCallback != nil {
					td.captureCallback(ByteRate(td.welford.EWMA()))
				}
			case stop := <-stopCapture:
				if stop {
					return
				}
			}
		}
	}(ticker)
	return stopCapture
}

func (dm *DataManager) NewChunk() Chunk {
	var dc DataChunk
	dc.manager = dm
	dm.Lock()
	*dm.Snapshot = append(*dm.Snapshot, &dc)
	dm.Unlock()
	return &dc
}

func (dm *DataManager) AddTotalDownload(value int64) {
	dm.download.AddTotalDataVolume(value)
}

func (dm *DataManager) AddTotalUpload(value int64) {
	dm.upload.AddTotalDataVolume(value)
}

func (dm *DataManager) GetTotalDownload() int64 {
	return dm.download.GetTotalDataVolume()
}

func (dm *DataManager) GetTotalUpload() int64 {
	return dm.upload.GetTotalDataVolume()
}

func (dm *DataManager) SetRateCaptureFrequency(duration time.Duration) Manager {
	dm.rateCaptureFrequency = duration
	return dm
}

func (dm *DataManager) SetCaptureTime(duration time.Duration) Manager {
	dm.captureTime = duration
	return dm
}

func (dm *DataManager) SetNThread(n int) Manager {
	if n < 1 {
		dm.nThread = runtime.NumCPU()
	} else {
		dm.nThread = n
	}
	return dm
}

func (dm *DataManager) Snapshots() *Snapshots {
	return dm.SnapshotStore
}

func (dm *DataManager) Reset() {
	dm.SnapshotStore.push(dm.Snapshot)
	dm.Snapshot = &Snapshot{}
	dm.download = dm.NewDataDirection(typeDownload)
	dm.upload = dm.NewDataDirection(typeUpload)
}

func (dm *DataManager) GetAvgDownloadRate() float64 {
	unit := float64(dm.captureTime / time.Millisecond)
	return float64(dm.download.GetTotalDataVolume()*8/1000) / unit
}

func (dm *DataManager) GetEWMADownloadRate() float64 {
	if dm.download.welford != nil {
		return dm.download.welford.EWMA()
	}
	return 0
}

func (dm *DataManager) GetAvgUploadRate() float64 {
	unit := float64(dm.captureTime / time.Millisecond)
	return float64(dm.upload.GetTotalDataVolume()*8/1000) / unit
}

func (dm *DataManager) NextUploadPayload() int64 {
	return dm.upload.ackMeter.nextPayload()
}

func (dm *DataManager) AckUpload(writeStart time.Time, written int64) {
	dm.upload.ackMeter.ack(writeStart, written)
}

func (dm *DataManager) GetAckedUploadRate() float64 {
	return dm.upload.ackMeter.rate()
}

func (dm *DataManager) GetEWMAUploadRate() float64 {
	if dm.upload.welford != nil {
		return dm.upload.welford.EWMA()
	}
	return 0
}

type DataChunk struct {
	manager             *DataManager
	dateType            DataType
	startTime           time.Time
	endTime             time.Time
	err                 error
	ContentLength       int64
	remainOrDiscardSize int64
	writeStart          time.Time
}

var blackHolePool = sync.Pool{
	New: func() any {
		b := make([]byte, 8192)
		return &b
	},
}

func (dc *DataChunk) GetDuration() time.Duration {
	return dc.endTime.Sub(dc.startTime)
}

func (dc *DataChunk) GetRate() float64 {
	if dc.dateType == typeDownload {
		return float64(dc.remainOrDiscardSize) / dc.GetDuration().Seconds()
	} else if dc.dateType == typeUpload {
		return float64(dc.ContentLength-dc.remainOrDiscardSize) * 8 / 1000 / 1000 / dc.GetDuration().Seconds()
	}
	return 0
}

// DownloadHandler No value will be returned here, because the error will interrupt the test.
// The error chunk is generally caused by the remote server actively closing the connection.
func (dc *DataChunk) DownloadHandler(r io.Reader) error {
	if dc.dateType != typeEmptyChunk {
		dc.err = errors.New("multiple calls to the same chunk handler are not allowed")
		return dc.err
	}
	dc.dateType = typeDownload
	dc.startTime = time.Now()
	defer func() {
		dc.endTime = time.Now()
	}()
	bufP := blackHolePool.Get().(*[]byte)
	defer blackHolePool.Put(bufP)
	readSize := 0
	for {
		dc.manager.runningRW.RLock()
		running := dc.manager.running
		dc.manager.runningRW.RUnlock()
		if !running {
			return nil
		}
		readSize, dc.err = r.Read(*bufP)
		rs := int64(readSize)

		dc.remainOrDiscardSize += rs
		dc.manager.download.AddTotalDataVolume(rs)
		if dc.err != nil {
			if dc.err == io.EOF {
				return nil
			}
			return dc.err
		}
	}
}

func (dc *DataChunk) UploadHandler(size int64) Chunk {
	if dc.dateType != typeEmptyChunk {
		dc.err = errors.New("multiple calls to the same chunk handler are not allowed")
	}

	if size <= 0 {
		panic("the size of repeated bytes should be > 0")
	}

	dc.ContentLength = size
	dc.remainOrDiscardSize = size
	dc.dateType = typeUpload
	dc.startTime = time.Now()
	return dc
}

func (dc *DataChunk) GetParent() Manager {
	return dc.manager
}

// WriteTo Used to hook all traffic.
// WriteSpan reports when this chunk began writing and how much it wrote. A
// chunk that never got to write reports a zero time and zero bytes.
func (dc *DataChunk) WriteSpan() (time.Time, int64) {
	return dc.writeStart, dc.ContentLength - dc.remainOrDiscardSize
}

func (dc *DataChunk) WriteTo(w io.Writer) (written int64, err error) {
	nw := 0
	nr := readChunkSize
	for {
		dc.manager.runningRW.RLock()
		running := dc.manager.running
		dc.manager.runningRW.RUnlock()
		// Finishing the payload, and being stopped part-way through it, are both
		// successful outcomes for this writer: the body is chunked, so a short
		// body is still a well-formed one. Reporting io.EOF here made net/http
		// treat every upload as a broken request, which skipped the terminating
		// chunk and cost the connection — so each request paid a fresh handshake
		// and restarted congestion control.
		if !running || dc.remainOrDiscardSize <= 0 {
			dc.endTime = time.Now()
			return written, nil
		}
		if dc.writeStart.IsZero() {
			// Rate is measured from the first byte on the wire, so that
			// connection setup is not charged to the link.
			dc.writeStart = time.Now()
			dc.manager.upload.ackMeter.noteWrite(dc.writeStart, dc.manager.captureTime)
		}
		if dc.remainOrDiscardSize < readChunkSize {
			nr = int(dc.remainOrDiscardSize)
			nw, err = w.Write((*dc.manager.repeatByte)[:nr])
		} else {
			nw, err = w.Write(*dc.manager.repeatByte)
		}
		if err != nil {
			return
		}
		n64 := int64(nw)
		written += n64
		dc.remainOrDiscardSize -= n64
		dc.manager.AddTotalUpload(n64)
		if nr != nw {
			return written, io.ErrShortWrite
		}
	}
}

// Please don't call it, only used to wrapped by [io.NopCloser]
// We use [DataChunk.WriteTo] that implements [io.WriterTo] to bypass this function.
func (dc *DataChunk) Read(b []byte) (n int, err error) {
	panic("unexpected call: only used to implement the io.Reader")
}

// calcMAFilter Median-Averaging Filter
func _(list []int64) float64 {
	if len(list) == 0 {
		return 0
	}
	var sum int64 = 0
	n := len(list)
	if n == 0 {
		return 0
	}
	length := len(list)
	for i := 0; i < length-1; i++ {
		for j := i + 1; j < length; j++ {
			if list[i] > list[j] {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	for i := 1; i < n-1; i++ {
		sum += list[i]
	}
	return float64(sum) / float64(n-2)
}

func pautaFilter(vector []int64) []int64 {
	dbg.Println("Per capture unit")
	dbg.Printf("Raw Sequence len: %d\n", len(vector))
	dbg.Printf("Raw Sequence: %v\n", vector)
	if len(vector) == 0 {
		return vector
	}
	mean, _, std, _, _ := sampleVariance(vector)
	var retVec []int64
	for _, value := range vector {
		if math.Abs(float64(value-mean)) < float64(3*std) {
			retVec = append(retVec, value)
		}
	}
	dbg.Printf("Raw average: %dByte\n", mean)
	dbg.Printf("Pauta Sequence len: %d\n", len(retVec))
	dbg.Printf("Pauta Sequence: %v\n", retVec)
	return retVec
}

// sampleVariance sample Variance
func sampleVariance(vector []int64) (mean, variance, stdDev, min, max int64) {
	if len(vector) == 0 {
		return 0, 0, 0, 0, 0
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
	variance = accumulate / int64(len(vector)-1) // Bessel's correction
	stdDev = int64(math.Sqrt(float64(variance)))
	return
}

const maxSnapshotSize = 10

type Snapshot []*DataChunk

type Snapshots struct {
	sp      []*Snapshot
	maxSize int
}

func newHistorySnapshots(size int) *Snapshots {
	return &Snapshots{
		sp:      make([]*Snapshot, 0, size),
		maxSize: size,
	}
}

func (rs *Snapshots) push(value *Snapshot) {
	if len(rs.sp) == rs.maxSize {
		rs.sp = rs.sp[1:]
	}
	rs.sp = append(rs.sp, value)
}

func (rs *Snapshots) Latest() *Snapshot {
	if len(rs.sp) > 0 {
		return rs.sp[len(rs.sp)-1]
	}
	return nil
}

func (rs *Snapshots) All() []*Snapshot {
	return rs.sp
}

func (rs *Snapshots) Clean() {
	rs.sp = make([]*Snapshot, 0, rs.maxSize)
}
