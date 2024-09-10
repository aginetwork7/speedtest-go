package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

func main() {
	var speedtestClient = speedtest.New(
		speedtest.WithUserConfig(
			&speedtest.UserConfig{
				PingMode:       speedtest.HTTP,
				Credential:     "abcdefg",
				MaxConnections: 8,
			},
		),
	)

	s, err := speedtestClient.CustomServer("http://dev-api.agi7.ai:10000")
	checkError(err)
	// Please make sure your host can access this test server,
	// otherwise you will get an error.
	// It is recommended to replace a server at this time
	start := time.Now()
	checkError(s.PingTestContext(context.Background(), func(latency time.Duration) {
		fmt.Println("Latency:", latency)
	}))
	fmt.Println("Ping cost:", time.Since(start))

	start = time.Now()
	var downCallbackSeq int32
	s.Context.SetCallbackDownload(func(downRate speedtest.ByteRate) {
		if downCallbackSeq%20 == 0 {
			fmt.Println("Download: ", int64(downRate.Mbps()*1000), "kbps")
		}
		downCallbackSeq++
	})
	checkError(s.DownloadTestContext(context.Background()))
	fmt.Println("Download cost:", time.Since(start))

	start = time.Now()
	var uploadCallbackSeq int32
	s.Context.SetCallbackUpload(func(upRate speedtest.ByteRate) {
		fmt.Println("Upload: ", upRate)
		if uploadCallbackSeq%20 == 0 {
			fmt.Println("Upload: ", int64(upRate.Mbps()*1000), "kbps")
		}
		uploadCallbackSeq++
	})
	checkError(s.UploadTestContext(context.Background()))
	fmt.Println("Upload cost:", time.Since(start))

	// Note: The unit of s.DLSpeed, s.ULSpeed is bytes per second, this is a float64.
	fmt.Printf("Latency: %s,Jitter %s, Download: %s, Upload: %s\n", s.Latency, s.Jitter, s.DLSpeed, s.ULSpeed)
	s.Context.Reset()
}

func checkError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
