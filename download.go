package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DownloadBlock defines download content
type DownloadBlock struct {
	offset      int64
	length      int64
	byteWritten int64
	errWritten  error
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
	progress map[int64]*DownloadRange
}

func NewDownloadProgress() *DownloadProgress {
	return &DownloadProgress{
		progress: make(map[int64]*DownloadRange),
	}
}

// addRange add a range to download progress
func (dp *DownloadProgress) addRange(start, end int64) {
	dp.Lock()
	dp.progress[start] = &DownloadRange{
		start:   start,
		end:     end,
		current: start,
	}
	dp.Unlock()
}

// removeRange remove a range from download progress
func (dp *DownloadProgress) removeRange(start, end int64) {
	dp.Lock()
	delete(dp.progress, start)
	dp.Unlock()
}

// updateRnage update a range in download progress
func (dp *DownloadProgress) updateRange(start, end, current int64) int64 {
	dp.Lock()
	defer dp.Unlock()
	dp.progress[start].current = current
	return dp.progress[start].end
}

// pickLargestUndownloadedRange pick the largest undownloaded range
func (dp *DownloadProgress) pickLargestUndownloadedRange() (start int64, end int64, ok bool) {
	dp.Lock()
	defer dp.Unlock()
	var maxRange *DownloadRange
	for _, r := range progress.progress {
		if maxRange == nil || r.end-r.current > maxRange.end-maxRange.current {
			//englishPrinter.Printf("\nfound a new range from %d to %d, current=%d, left size=%d\n", r.start, r.end, r.current, r.end-r.current)
			maxRange = r
		}
	}
	if maxRange == nil || maxRange.end-maxRange.current < leastTryBufferSize {
		return 0, 0, false
	}

	end = maxRange.end
	start = maxRange.current + (maxRange.end-maxRange.current)/2
	dp.progress[start] = &DownloadRange{
		start:   start,
		end:     end,
		current: start,
	}
	//englishPrinter.Printf("\nresize origin undownloaded range from %d-%d to %d-%d\n", maxRange.start, maxRange.end, maxRange.start, start-1)
	//englishPrinter.Printf("\npick new undownloaded range from %d-%d, %d ranges left\n", start, end, len(dp.progress))
	maxRange.end = start

	return start, end, true
}

var (
	progress = NewDownloadProgress()
	fd       *os.File
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

	buf := make([]byte, readBufSize)
	offset := min
start:
	client := getHTTPClient(isHTTP3)
	resp, err := client.Do(req)
	if err != nil {
		if retryTimes < 0 || retry < retryTimes {
			//englishPrinter.Printf("request bytes=%d-%d error: %+v, retry it %d time\n", min, max-1, err, retry)
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
			nr, er := resp.Body.Read(buf)
			if nr > 0 {
				if offset+int64(nr) > max {
					nr = int(max - offset)
				}
				nw, ew := fd.WriteAt(buf[:nr], offset)
				output <- DownloadBlock{
					offset:      offset,
					length:      int64(nr),
					byteWritten: int64(nw),
					errWritten:  ew,
				}
				if ew != nil {
					err = ew
					goto exit
				}
				offset += int64(nr)
				max = progress.updateRange(min, max, offset)
				if reuseThread && offset >= max {
					progress.removeRange(min, max)
					if newMin, newMax, ok := progress.pickLargestUndownloadedRange(); ok {
						//englishPrinter.Printf("\nend a block from %d to %d, total received bytes: %d, start new block from %d to %d\n", min, max, offset-min, newMin, newMax)
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
						logs := englishPrinter.Sprintf("request bytes=%d-%d received %d bytes but got error: %+v, retry it %d time\n", min, max-1, offset-min, er, retry)
						logStdout.Println(logs)
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
	logs := englishPrinter.Sprintf("\nend a thread from %d to %d, total received bytes: %d\n", min, max, offset-min)
	logStdout.Println(logs)
	done <- err
	return err
}

func downloadFileRequest(uri string, contentLength int64, filePath string, isHTTP3 bool) error {
	tsBegin := time.Now()

	dir := filepath.Dir(filePath)
	_, err := os.Stat(dir)
	if os.ErrNotExist == err {
		os.MkdirAll(dir, 0755)
	}

	fd, err = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		logStderr.Println(err)
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
			nw := b.byteWritten
			if nw > 0 {
				totalReceived += int64(nw)
			}
			ew := b.errWritten
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
			logs := englishPrinter.Sprintf("\rreceived and wrote %d/%d bytes to offset %d in %+v at %d B/s", totalReceived, contentLength, b.offset, tsCost, speed)
			logStdout.Println(logs)
		case err = <-done:
			i++
			logStdout.Println("\n%d/%d thread is ended.\n", i, concurrentThread)
		}
	}

	cancel()
	fmt.Printf("\n")
	if err != nil && err != io.EOF {
		logStderr.Println(err)
	} else {
		tsEnd := time.Now()
		tsCost := tsEnd.Sub(tsBegin)
		speed := totalReceived * 1000 / int64(tsCost/time.Millisecond)
		logs := englishPrinter.Sprintf("%d bytes received and written to %s in %+v at %d B/s\n", totalReceived, filePath, tsCost, speed)
		logStdout.Println(logs)
	}
	return err
}
