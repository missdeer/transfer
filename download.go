package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	readBufSize              = 32 * 1024
	leastTryBufferSize int64 = 256 * 1024
)

// DownloadBlock defines download content
type DownloadBlock struct {
	offset int64
	length int64
	buf    []byte
}

// DownloadRange defines download progress in a block
type DownloadRange struct {
	start   int64
	end     int64
	current int64
}

// DownloadProgress defines download progress total
type DownloadProgress struct {
	sync.Mutex
	progress []*DownloadRange
}

// addRange add a range to download progress
func (dp *DownloadProgress) addRange(start, end int64) {
	dp.Lock()
	dp.progress = append(dp.progress, &DownloadRange{
		start:   start,
		end:     end,
		current: 0,
	})
	dp.Unlock()
}

// updateRnage update a range in download progress
func (dp *DownloadProgress) updateRange(start, end, current int64) {
	dp.Lock()
	for _, r := range dp.progress {
		if r.start == start && r.end == end {
			r.current = current
			break
		}
	}
	dp.Unlock()
}

// pickLargestUndownloadedRange pick the largest undownloaded range
func (dp *DownloadProgress) pickLargestUndownloadedRange() (start int64, end int64, ok bool) {
	dp.Lock()
	defer dp.Unlock()
	var maxRange *DownloadRange
	for _, r := range progress.progress {
		if maxRange == nil || r.end-r.current > maxRange.end-maxRange.current {
			maxRange = r
		}
	}
	if maxRange.end-maxRange.current < leastTryBufferSize {
		return 0, 0, false
	}

	end = maxRange.end
	start = maxRange.current + (maxRange.end-maxRange.current)/2 + 1
	dp.progress = append(dp.progress, &DownloadRange{
		start:   start,
		end:     end,
		current: 0,
	})
	maxRange.end = start - 1

	return start, end, true
}

var (
	progress DownloadProgress
)

func downloadFileRequestAt(ctx context.Context, uri string, min int64, max int64, isHTTP3 bool, output chan DownloadBlock, done chan error) error {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		done <- err
		return err
	}
	retry := 1
	rangeHeader := fmt.Sprintf("bytes=%d-%d", min, max-1) // Add the data for the Range header of the form "bytes=0-100"
	req.Header.Add("Range", rangeHeader)

	offset := min
start:
	client := getHTTPClient(isHTTP3)
	resp, err := client.Do(req)
	if err != nil {
		if retryTimes < 0 || retry < retryTimes {
			englishPrinter.Printf("request bytes=%d-%d error: %+v, retry it %d time\n", min, max-1, err, retry)
			retry++
			goto start
		}
		goto exit
	}
	defer resp.Body.Close()
	for {
		select {
		case <-ctx.Done():
			goto exit
		default:
			buf := make([]byte, readBufSize)
			nr, er := resp.Body.Read(buf)
			if nr > 0 {
				output <- DownloadBlock{
					offset: offset,
					length: int64(nr),
					buf:    buf[0:nr],
				}
				offset += int64(nr)
				progress.updateRange(min, max, offset)
				if reuseThread && offset >= max {
					if newMin, newMax, ok := progress.pickLargestUndownloadedRange(); ok {
						englishPrinter.Printf("\nend a block from %d to %d, total received bytes: %d, start new block from %d to %d\n", min, max, offset-min, newMin, newMax)
						downloadFileRequestAt(ctx, uri, newMin, newMax, isHTTP3, output, done)
						return nil
					} else {
						err = er
						goto exit
					}
				}
			}
			if er != nil {
				if er != io.EOF {
					if retryTimes < 0 || retry < retryTimes {
						englishPrinter.Printf("request bytes=%d-%d received %d bytes but got error: %+v, retry it %d time\n", min, max-1, offset-min, er, retry)
						retry++
						req, err = http.NewRequest("GET", uri, nil)
						if err != nil {
							goto exit
						}
						rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, max-1) // fix new requested range
						req.Header.Add("Range", rangeHeader)
						goto start
					}
					err = er
				}
				goto exit
			}
		}
	}
exit:
	englishPrinter.Printf("\nend a thread from %d to %d, total received bytes: %d\n", min, max, offset-min)
	done <- err
	return err
}

func downloadFileRequest(uri string, contentLength int64, filePath string, isHTTP3 bool) error {
	tsBegin := time.Now()

	dir := filepath.Dir(filePath)
	if _, err := os.Stat(dir); os.ErrNotExist == err {
		os.MkdirAll(dir, 0755)
	}

	fd, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Println(err)
		return err
	}
	defer fd.Close()
	if contentLength > 0 {
		fd.Truncate(contentLength)
	} else {
		concurrentThread = 1
	}

	ctxt, cancel := context.WithCancel(context.Background())

	lenSub := contentLength / int64(concurrentThread) // Bytes for each Go-routine
	diff := contentLength % int64(concurrentThread)   // Get the remaining for the last request
	output := make(chan DownloadBlock)
	done := make(chan error)
	for i := 0; i < concurrentThread; i++ {
		min := lenSub * int64(i) // Min range
		max := min + lenSub      // Max range

		if i == concurrentThread-1 {
			max += diff // Add the remaining bytes in the last request
		}

		ctxtChild, _ := context.WithCancel(ctxt)
		progress.addRange(min, max)
		go downloadFileRequestAt(ctxtChild, uri, min, max, isHTTP3, output, done)
	}

	var totalReceived int64
	for i := 0; i < concurrentThread && (err == nil || err == io.EOF); {
		select {
		case b := <-output:
			nw, ew := fd.WriteAt(b.buf[0:int(b.length)], b.offset)
			if nw > 0 {
				totalReceived += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if b.length != int64(nw) {
				err = io.ErrShortWrite
				break
			}
			tsEnd := time.Now()
			tsCost := tsEnd.Sub(tsBegin)
			speed := totalReceived * 1000 / int64(tsCost/time.Millisecond)
			englishPrinter.Printf("\rreceived and wrote %d/%d bytes to offset %d in %+v at %d B/s", totalReceived, contentLength, b.offset, tsCost, speed)
		case err = <-done:
			i++
			fmt.Printf("\n%d/%d thread is ended.\n", i, concurrentThread)
		}
	}

	cancel()
	fmt.Printf("\n")
	if err != nil && err != io.EOF {
		log.Println(err)
	} else {
		tsEnd := time.Now()
		tsCost := tsEnd.Sub(tsBegin)
		speed := totalReceived * 1000 / int64(tsCost/time.Millisecond)
		logs := englishPrinter.Sprintf("%d bytes received and written to %s in %+v at %d B/s\n", totalReceived, filePath, tsCost, speed)
		log.Printf(logs)
	}
	return err
}
