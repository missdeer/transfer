package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errDownloadStalled        = errors.New("download connection stalled")
	workStealPollInterval     = time.Second
	workStealStallTimeout     = 5 * time.Second
	workStealSlowSampleWindow = 5 * time.Second
	workStealLowSpeed         = int64(64 * 1024)
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
	start          int64
	end            int64
	current        int64
	lastProgressAt time.Time
	sampledAt      time.Time
	sampledCurrent int64
	recentSpeed    float64
	speedMeasured  bool
}

// DownloadProgress defines download progress total
type DownloadProgress struct {
	sync.Mutex
	ranges       map[int64]*DownloadRange
	minStealSize int64
	minSlowSteal int64
	pollInterval time.Duration
	stallTimeout time.Duration
	slowWindow   time.Duration
	lowSpeed     int64
	changed      chan struct{}
}

func NewDownloadProgress(minStealSize int64) *DownloadProgress {
	if minStealSize < 2 {
		minStealSize = 2
	}
	minSlowSteal := readBufSize * 2
	if minSlowSteal < 2 {
		minSlowSteal = 2
	}
	return &DownloadProgress{
		ranges:       make(map[int64]*DownloadRange),
		minStealSize: minStealSize,
		minSlowSteal: minSlowSteal,
		pollInterval: workStealPollInterval,
		stallTimeout: workStealStallTimeout,
		slowWindow:   workStealSlowSampleWindow,
		lowSpeed:     workStealLowSpeed,
		changed:      make(chan struct{}),
	}
}

// addRange add a range to download progress
func (dp *DownloadProgress) addRange(start, end int64) {
	dp.Lock()
	now := time.Now()
	dp.ranges[start] = &DownloadRange{
		start:          start,
		end:            end,
		current:        start,
		lastProgressAt: now,
		sampledAt:      now,
		sampledCurrent: start,
	}
	dp.notifyLocked()
	dp.Unlock()
}

// removeRange remove a range from download progress
func (dp *DownloadProgress) removeRange(start int64) {
	dp.Lock()
	if _, ok := dp.ranges[start]; ok {
		delete(dp.ranges, start)
		dp.notifyLocked()
	}
	dp.Unlock()
}

func (dp *DownloadProgress) notifyLocked() {
	close(dp.changed)
	dp.changed = make(chan struct{})
}

// updateRange records a worker's progress and returns its current end. The end
// may move backwards when another worker steals the tail of this range.
func (dp *DownloadProgress) updateRange(start, current int64) (int64, bool) {
	dp.Lock()
	defer dp.Unlock()
	r, ok := dp.ranges[start]
	if !ok {
		return 0, false
	}
	if current > r.current {
		now := time.Now()
		r.lastProgressAt = now
		if elapsed := now.Sub(r.sampledAt); elapsed >= dp.slowWindow && elapsed > 0 {
			r.recentSpeed = float64(current-r.sampledCurrent) / elapsed.Seconds()
			r.speedMeasured = true
			r.sampledAt = now
			r.sampledCurrent = current
		}
	}
	r.current = current
	return r.end, true
}

// stealRange splits the largest unfinished range in half and returns its tail.
// The worker that owns the original range observes the shortened end through
// updateRange and stops there, so the two workers never intentionally overlap.
func (dp *DownloadProgress) stealRange() (start int64, end int64, ok bool) {
	dp.Lock()
	defer dp.Unlock()
	var maxRange *DownloadRange
	for _, r := range dp.ranges {
		if maxRange == nil || r.end-r.current > maxRange.end-maxRange.current {
			maxRange = r
		}
	}
	if maxRange == nil || maxRange.end-maxRange.current < dp.minStealSize {
		return 0, 0, false
	}

	end = maxRange.end
	start = maxRange.current + (maxRange.end-maxRange.current)/2
	dp.addStolenRangeLocked(start, end)
	maxRange.end = start

	return start, end, true
}

func (dp *DownloadProgress) addStolenRangeLocked(start, end int64) {
	now := time.Now()
	dp.ranges[start] = &DownloadRange{
		start:          start,
		end:            end,
		current:        start,
		lastProgressAt: now,
		sampledAt:      now,
		sampledCurrent: start,
	}
	dp.notifyLocked()
}

// stealSlowRange uses a smaller split threshold for a connection that has
// stopped making progress or remained below the low-speed threshold long
// enough. This lets an idle worker open a fresh connection for the tail.
func (dp *DownloadProgress) stealSlowRange(now time.Time) (start, end int64, ok bool) {
	dp.Lock()
	defer dp.Unlock()

	var candidate *DownloadRange
	for _, r := range dp.ranges {
		remaining := r.end - r.current
		if remaining < dp.minSlowSteal {
			continue
		}
		stalled := now.Sub(r.lastProgressAt) >= dp.stallTimeout
		slow := r.speedMeasured && r.recentSpeed < float64(dp.lowSpeed)
		if !stalled && !slow {
			continue
		}
		if candidate == nil || remaining > candidate.end-candidate.current {
			candidate = r
		}
	}
	if candidate == nil {
		return 0, 0, false
	}

	end = candidate.end
	start = candidate.current + (candidate.end-candidate.current)/2
	dp.addStolenRangeLocked(start, end)
	candidate.end = start
	return start, end, true
}

func (dp *DownloadProgress) rangeState() (bool, <-chan struct{}) {
	dp.Lock()
	defer dp.Unlock()
	return len(dp.ranges) > 0, dp.changed
}

// waitAndSteal keeps an otherwise idle worker available while other workers
// still own ranges. It first uses normal work stealing, then periodically
// checks whether a stalled or slow connection should be split more eagerly.
func (dp *DownloadProgress) waitAndSteal(ctx context.Context) (DownloadRange, bool) {
	for {
		if start, end, ok := dp.stealRange(); ok {
			return DownloadRange{start: start, end: end, current: start}, true
		}
		hasRanges, changed := dp.rangeState()
		if !hasRanges {
			return DownloadRange{}, false
		}

		timer := time.NewTimer(dp.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return DownloadRange{}, false
		case <-changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case now := <-timer.C:
			if start, end, ok := dp.stealSlowRange(now); ok {
				return DownloadRange{start: start, end: end, current: start}, true
			}
		}
	}
}

func downloadFileRequestAt(ctx context.Context, uri string, min, max int64, isHTTP3 bool, file *os.File, progress *DownloadProgress, output chan<- DownloadBlock) error {
	buf := make([]byte, readBufSize)
	offset := min
	attempt := 1
	unbounded := max < 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !unbounded {
			currentEnd, ok := progress.updateRange(min, offset)
			if !ok || offset >= currentEnd {
				return nil
			}
			max = currentEnd
		}

		requestCtx, cancelRequest := context.WithCancel(ctx)
		var requestStalled atomic.Bool
		stallTimeout := workStealStallTimeout
		if progress != nil {
			stallTimeout = progress.stallTimeout
		}
		stallTimer := time.AfterFunc(stallTimeout, func() {
			requestStalled.Store(true)
			cancelRequest()
		})
		stopRequest := func() {
			stallTimer.Stop()
			cancelRequest()
		}

		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, uri, nil)
		if err != nil {
			stopRequest()
			return err
		}
		SetRequestHeader(req)
		if !unbounded {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, max-1))
		}

		resp, err := getHTTPClient(isHTTP3).Do(req)
		if err != nil {
			stopRequest()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if requestStalled.Load() {
				err = errDownloadStalled
			}
			if retryTimes < 0 || attempt < retryTimes {
				attempt++
				continue
			}
			return err
		}

		for {
			nr, readErr := resp.Body.Read(buf)
			if nr > 0 {
				stallTimer.Reset(stallTimeout)
				if !unbounded {
					currentEnd, ok := progress.updateRange(min, offset)
					if !ok || offset >= currentEnd {
						resp.Body.Close()
						stopRequest()
						return nil
					}
					max = currentEnd
					if offset+int64(nr) > max {
						nr = int(max - offset)
					}
				}

				if nr > 0 {
					nw := nr
					var writeErr error
					if file != nil {
						nw, writeErr = file.WriteAt(buf[:nr], offset)
					}
					block := DownloadBlock{
						offset:      offset,
						length:      int64(nr),
						byteWritten: int64(nw),
						errWritten:  writeErr,
					}
					select {
					case output <- block:
					case <-ctx.Done():
						resp.Body.Close()
						stopRequest()
						return ctx.Err()
					}
					if writeErr != nil {
						resp.Body.Close()
						stopRequest()
						return writeErr
					}
					if nw != nr {
						resp.Body.Close()
						stopRequest()
						return io.ErrShortWrite
					}

					offset += int64(nr)
					if !unbounded {
						currentEnd, ok := progress.updateRange(min, offset)
						if !ok || offset >= currentEnd {
							resp.Body.Close()
							stopRequest()
							return nil
						}
						max = currentEnd
					}
				}
			}

			if readErr == nil {
				continue
			}
			resp.Body.Close()
			stopRequest()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if requestStalled.Load() {
				readErr = errDownloadStalled
			}
			if unbounded && readErr == io.EOF {
				return nil
			}
			if readErr == io.EOF && offset >= max {
				return nil
			}
			if readErr == io.EOF {
				readErr = io.ErrUnexpectedEOF
			}
			if retryTimes < 0 || attempt < retryTimes {
				logs := englishPrinter.Sprintf("\nrequest bytes=%d-%d received %d bytes but got error: %+v, retry it %d time\n", min, max-1, offset-min, readErr, attempt)
				logStdout.Println(logs)
				attempt++
				break
			}
			return readErr
		}
	}
}

func downloadWorker(ctx context.Context, uri string, work DownloadRange, isHTTP3 bool, file *os.File, progress *DownloadProgress, output chan<- DownloadBlock, reuse bool) error {
	for {
		start, end := work.start, work.end
		err := downloadFileRequestAt(ctx, uri, start, end, isHTTP3, file, progress, output)
		if progress != nil {
			progress.removeRange(start)
		}
		logs := englishPrinter.Sprintf("\nend a block from %d to %d\n", start, end)
		logStdout.Println(logs)
		if err != nil {
			return err
		}
		if !reuse || progress == nil {
			return nil
		}
		newWork, ok := progress.waitAndSteal(ctx)
		if !ok {
			return nil
		}
		work = newWork
	}
}

func downloadFileRequest(uri string, contentLength int64, filePath string, isHTTP3 bool) error {
	tsBegin := time.Now()

	dir := filepath.Dir(filePath)
	_, err := os.Stat(dir)
	if os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	var file *os.File
	if (runtime.GOOS == "windows" && filePath == "NUL") || (runtime.GOOS != "windows" && filePath == "/dev/null") {
		logStdout.Println("write to blackhole")
	} else {
		file, err = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			logStderr.Println(err)
			return err
		}
	}
	defer func() {
		if file != nil {
			file.Close()
		}
	}()
	if contentLength > 0 {
		if file != nil {
			if err = file.Truncate(contentLength); err != nil {
				return err
			}
		}
	}

	if concurrentThread < 1 {
		return fmt.Errorf("download thread count must be positive")
	}

	workerCount := concurrentThread
	if contentLength > 0 && int64(workerCount) > contentLength {
		workerCount = int(contentLength)
	}
	if contentLength <= 0 {
		workerCount = 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(chan DownloadBlock)
	done := make(chan error, workerCount)

	var progress *DownloadProgress
	if contentLength > 0 {
		progress = NewDownloadProgress(leastTryBufferSize)
		lenSub := contentLength / int64(workerCount)
		diff := contentLength % int64(workerCount)
		works := make([]DownloadRange, workerCount)
		for i := 0; i < workerCount; i++ {
			min := lenSub * int64(i)
			max := min + lenSub
			if i == workerCount-1 {
				max += diff
			}
			progress.addRange(min, max)
			works[i] = DownloadRange{start: min, end: max, current: min}
		}
		for _, work := range works {
			go func() {
				done <- downloadWorker(ctx, uri, work, isHTTP3, file, progress, output, reuseThread)
			}()
		}
	} else {
		work := DownloadRange{start: 0, end: -1, current: 0}
		go func() {
			done <- downloadWorker(ctx, uri, work, isHTTP3, file, nil, output, false)
		}()
	}

	var totalReceived int64
	var firstErr error
	for completed := 0; completed < workerCount; {
		select {
		case b := <-output:
			if b.byteWritten > 0 {
				totalReceived += b.byteWritten
			}
			if b.errWritten != nil && firstErr == nil {
				firstErr = b.errWritten
				cancel()
			}
			if b.length != b.byteWritten && firstErr == nil {
				firstErr = io.ErrShortWrite
				cancel()
			}
			tsCost := time.Since(tsBegin)
			elapsedMilliseconds := tsCost.Milliseconds()
			if elapsedMilliseconds < 1 {
				elapsedMilliseconds = 1
			}
			speed := totalReceived * 1000 / elapsedMilliseconds
			englishPrinter.Printf("\rreceived and wrote %d/%d bytes to offset %d in %+v at %d B/s", totalReceived, contentLength, b.offset, tsCost, speed)
		case workerErr := <-done:
			completed++
			if workerErr != nil && workerErr != context.Canceled && firstErr == nil {
				firstErr = workerErr
				cancel()
			}
			logStdout.Printf("\n%d/%d thread is ended.\n", completed, workerCount)
		}
	}

	fmt.Printf("\n")
	if firstErr != nil {
		logStderr.Println(firstErr)
	} else {
		tsCost := time.Since(tsBegin)
		elapsedMilliseconds := tsCost.Milliseconds()
		if elapsedMilliseconds < 1 {
			elapsedMilliseconds = 1
		}
		speed := totalReceived * 1000 / elapsedMilliseconds
		logs := englishPrinter.Sprintf("%d bytes received and written to %s in %+v at %d B/s\n", totalReceived, filePath, tsCost, speed)
		logStdout.Println(logs)
	}
	return firstErr
}
