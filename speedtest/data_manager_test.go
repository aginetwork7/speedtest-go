package speedtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkDataManager_NewDataChunk(b *testing.B) {
	dmp := NewDataManager()
	dmp.Reset()
	for i := 0; i < b.N; i++ {
		dmp.NewChunk()
	}
}

func BenchmarkDataManager_AddTotalDownload(b *testing.B) {
	dmp := NewDataManager()
	for i := 0; i < b.N; i++ {
		dmp.AddTotalDownload(43521)
	}
}

func TestDataManager_AddTotalDownload(t *testing.T) {
	dmp := NewDataManager()
	wg := sync.WaitGroup{}
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			for j := 0; j < 1000; j++ {
				dmp.AddTotalDownload(43521)
			}
			wg.Done()
		}()
	}
	wg.Wait()
	if dmp.download.GetTotalDataVolume() != 43521000000 {
		t.Fatal()
	}
}

func TestDataManager_GetAvgDownloadRate(t *testing.T) {
	dm := NewDataManager()
	dm.download.totalDataVolume = 3000000
	dm.captureTime = time.Second * 10

	result := dm.GetAvgDownloadRate()
	if result != 2.4 {
		t.Fatal()
	}
}

func TestDynamicRate(t *testing.T) {

	server, _ := CustomServer("http://shenzhen.cmcc.speedtest.shunshiidc.com:8080/speedtest/upload.php")
	//server, _ := CustomServer("http://192.168.5.237:8080/speedtest/upload.php")

	oldDownTotal := server.Context.Manager.GetTotalDownload()
	oldUpTotal := server.Context.Manager.GetTotalUpload()

	server.Context.Manager.SetRateCaptureFrequency(time.Millisecond * 100)
	server.Context.Manager.SetCaptureTime(time.Second)
	go func() {
		for i := 0; i < 2; i++ {
			time.Sleep(time.Second)
			newDownTotal := server.Context.Manager.GetTotalDownload()
			newUpTotal := server.Context.Manager.GetTotalUpload()

			downRate := float64(newDownTotal-oldDownTotal) * 8 / 1000 / 1000
			upRate := float64(newUpTotal-oldUpTotal) * 8 / 1000 / 1000
			oldDownTotal = newDownTotal
			oldUpTotal = newUpTotal
			fmt.Printf("downRate: %.2fMbps | upRate: %.2fMbps\n", downRate, upRate)
		}
	}()

	err := server.DownloadTest()
	if err != nil {
		fmt.Println("Warning: not found server")
		//t.Error(err)
	}

	server.Context.Manager.Wait()

	err = server.UploadTest()
	if err != nil {
		fmt.Println("Warning: not found server")
		//t.Error(err)
	}

	fmt.Printf(" \n")

	fmt.Printf("Download: %5.2f Mbit/s\n", server.DLSpeed)
	fmt.Printf("Upload: %5.2f Mbit/s\n\n", server.ULSpeed)
	valid := server.CheckResultValid()
	if !valid {
		fmt.Println("Warning: result seems to be wrong. Please test again.")
	}
}

// Handlers a caller registers through the exported API belong to that caller.
// Start used to drop them on its way out, so starting the same direction a
// second time panicked on an empty task stack.
func TestStartRunsCallerHandlersAgain(t *testing.T) {
	client := New()
	client.SetCaptureTime(100 * time.Millisecond)
	client.SetNThread(1)
	var calls atomic.Int64
	td := client.RegisterDownloadHandler(func() {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
	})

	for run := 1; run <= 2; run++ {
		before := calls.Load()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("run %d of the same direction panicked: %v", run, r)
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			td.Start(ctx, cancel, 0)
		}()
		if calls.Load() == before {
			t.Fatalf("run %d did not call the registered handler", run)
		}
	}
}

// A direction's byte counter runs for the life of the client, while each run
// starts a fresh rate capture. The capture has to start from the counter's
// value at that moment: starting from zero fed every byte of earlier phases
// into the first sample, so a second measurement on the same client read
// several times the real rate.
func TestDownloadRateIgnoresEarlierPhasesBytes(t *testing.T) {
	measure := func(earlier int64) float64 {
		dm := NewDataManager()
		dm.SetCaptureTime(time.Second)
		dm.SetNThread(1)
		dm.AddTotalDownload(earlier)
		steady := func() {
			dm.AddTotalDownload(10_000)
			time.Sleep(10 * time.Millisecond)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		dm.download.run(ctx, cancel, 0, []func(){steady})
		return dm.GetEWMADownloadRate()
	}

	fresh := measure(0)
	after := measure(500 << 20)
	if fresh <= 0 {
		t.Fatalf("the steady handler measured no rate, so the comparison means nothing")
	}
	if after > fresh*1.5 {
		t.Fatalf("after 500 MiB of earlier phases the rate read %.0f B/s, against %.0f B/s on a fresh client", after, fresh)
	}
}
