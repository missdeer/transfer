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
	initialDownloadChunkSize  = int64(16 * 1024 * 1024)
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
	start           int64
	end             int64
	current         int64
	sourceInterface string
	lastProgressAt  time.Time
	sampledAt       time.Time
	sampledCurrent  int64
	recentSpeed     float64
	speedMeasured   bool
}

// DownloadProgress defines download progress total
type DownloadProgress struct {
	sync.Mutex
	ranges       map[int64]*DownloadRange
	sources      map[string]*interfaceThroughput
	sourceLimits map[string]int
	pendingStart int64
	pendingEnd   int64
	chunkSize    int64
	minStealSize int64
	minSlowSteal int64
	pollInterval time.Duration
	stallTimeout time.Duration
	slowWindow   time.Duration
	lowSpeed     int64
	changed      chan struct{}
}

type interfaceThroughput struct {
	bytes        int64
	sampledBytes int64
	sampledAt    time.Time
	rate         float64
	measured     bool
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
		sources:      make(map[string]*interfaceThroughput),
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
	dp.addRangeOnInterface(start, end, "")
}

func (dp *DownloadProgress) addRangeOnInterface(start, end int64, sourceInterface string) {
	dp.Lock()
	now := time.Now()
	dp.ensureSourceLocked(sourceInterface, now)
	dp.ranges[start] = &DownloadRange{
		start:           start,
		end:             end,
		current:         start,
		sourceInterface: sourceInterface,
		lastProgressAt:  now,
		sampledAt:       now,
		sampledCurrent:  start,
	}
	dp.notifyLocked()
	dp.Unlock()
}

func (dp *DownloadProgress) ensureSourceLocked(sourceInterface string, now time.Time) {
	if dp.sources[sourceInterface] == nil {
		dp.sources[sourceInterface] = &interfaceThroughput{sampledAt: now}
	}
}

func (dp *DownloadProgress) setPendingRange(start, end, chunkSize int64) {
	dp.Lock()
	dp.pendingStart = start
	dp.pendingEnd = end
	dp.chunkSize = chunkSize
	dp.notifyLocked()
	dp.Unlock()
}

func (dp *DownloadProgress) setSourceLimits(interfaces []string, counts []int) {
	dp.Lock()
	dp.sourceLimits = make(map[string]int, len(interfaces))
	for i, sourceInterface := range interfaces {
		dp.sourceLimits[sourceInterface] = counts[i]
	}
	dp.notifyLocked()
	dp.Unlock()
}

func (dp *DownloadProgress) takePendingRangeForInterfaces(interfaces []string) (DownloadRange, bool) {
	dp.Lock()
	defer dp.Unlock()
	if dp.pendingStart >= dp.pendingEnd {
		return DownloadRange{}, false
	}
	start := dp.pendingStart
	end := dp.pendingEnd
	if end-start > dp.chunkSize {
		end = start + dp.chunkSize
	}
	sourceInterface := dp.selectLeastBusyInterfaceLocked(interfaces, "")
	if len(dp.sourceLimits) > 0 && sourceInterface == "" {
		return DownloadRange{}, false
	}
	dp.pendingStart = end
	dp.addStolenRangeLocked(start, end, sourceInterface)
	return DownloadRange{start: start, end: end, current: start, sourceInterface: sourceInterface}, true
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
		dp.sources[r.sourceInterface].bytes += current - r.current
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
	start, end, _, ok = dp.stealRangeForInterfaces(nil)
	return
}

func (dp *DownloadProgress) stealRangeForInterfaces(interfaces []string) (start int64, end int64, sourceInterface string, ok bool) {
	dp.Lock()
	defer dp.Unlock()
	finishTimes, measured := dp.estimatedFinishTimesLocked(interfaces, time.Now())
	var maxRange *DownloadRange
	for _, r := range dp.ranges {
		remaining := r.end - r.current
		if remaining < dp.minStealSize {
			continue
		}
		if maxRange == nil ||
			(measured && finishTimes[r.sourceInterface] > finishTimes[maxRange.sourceInterface]) ||
			(finishTimes[r.sourceInterface] == finishTimes[maxRange.sourceInterface] && remaining > maxRange.end-maxRange.current) {
			maxRange = r
		}
	}
	if maxRange == nil {
		return 0, 0, "", false
	}

	sourceInterface = dp.selectLeastBusyInterfaceLocked(interfaces, maxRange.sourceInterface)
	if len(dp.sourceLimits) > 0 && sourceInterface == "" {
		return 0, 0, "", false
	}
	end = maxRange.end
	start = maxRange.current + (maxRange.end-maxRange.current)/2
	if measured && sourceInterface != maxRange.sourceInterface {
		sourceRate := dp.sources[maxRange.sourceInterface].rate
		targetRate := dp.sources[sourceInterface].rate
		if targetRate > 0 {
			move := float64(1 << 62)
			if sourceRate > 0 {
				sourceRemaining := finishTimes[maxRange.sourceInterface] * sourceRate
				targetRemaining := finishTimes[sourceInterface] * targetRate
				move = (targetRate*sourceRemaining - sourceRate*targetRemaining) / (sourceRate + targetRate)
			}
			if move < float64(dp.minStealSize)/2 {
				return 0, 0, "", false
			}
			maxMove := maxRange.end - maxRange.current - 1
			if move > float64(maxMove) {
				move = float64(maxMove)
			}
			start = end - int64(move)
		}
	}
	dp.addStolenRangeLocked(start, end, sourceInterface)
	maxRange.end = start

	return start, end, sourceInterface, true
}

func (dp *DownloadProgress) selectLeastBusyInterfaceLocked(interfaces []string, fallback string) string {
	if len(interfaces) == 0 {
		return fallback
	}
	counts := make([]int, len(interfaces))
	for _, r := range dp.ranges {
		for i, sourceInterface := range interfaces {
			if r.sourceInterface == sourceInterface {
				counts[i]++
				break
			}
		}
	}
	available := func(i int) bool {
		return len(dp.sourceLimits) == 0 || counts[i] < dp.sourceLimits[interfaces[i]]
	}
	finishTimes, measured := dp.estimatedFinishTimesLocked(interfaces, time.Now())
	if measured {
		selected := ""
		for i, sourceInterface := range interfaces {
			if available(i) && dp.sources[sourceInterface].rate > 0 && (selected == "" || finishTimes[sourceInterface] < finishTimes[selected]) {
				selected = sourceInterface
			}
		}
		if selected != "" {
			if fallback != "" && dp.sourceLimitAvailableLocked(fallback, counts, interfaces) && finishTimes[fallback] <= finishTimes[selected]*1.1 {
				return fallback
			}
			return selected
		}
	}
	selected := -1
	for i := range counts {
		if !available(i) {
			continue
		}
		if selected < 0 || counts[i] < counts[selected] ||
			(counts[i] == counts[selected] && interfaces[selected] == fallback && interfaces[i] != fallback) {
			selected = i
		}
	}
	if selected < 0 {
		return ""
	}
	return interfaces[selected]
}

func (dp *DownloadProgress) sourceLimitAvailableLocked(sourceInterface string, counts []int, interfaces []string) bool {
	for i, source := range interfaces {
		if source == sourceInterface {
			return len(dp.sourceLimits) == 0 || counts[i] < dp.sourceLimits[source]
		}
	}
	return false
}

func (dp *DownloadProgress) estimatedFinishTimesLocked(interfaces []string, now time.Time) (map[string]float64, bool) {
	if len(interfaces) == 0 {
		return nil, false
	}
	remaining := make(map[string]int64, len(interfaces))
	active := make(map[string]int, len(interfaces))
	for _, r := range dp.ranges {
		remaining[r.sourceInterface] += r.end - r.current
		active[r.sourceInterface]++
	}
	finishTimes := make(map[string]float64, len(interfaces))
	for _, sourceInterface := range interfaces {
		throughput := dp.sources[sourceInterface]
		if throughput == nil {
			return nil, false
		}
		if elapsed := now.Sub(throughput.sampledAt); elapsed >= time.Second {
			instant := float64(throughput.bytes-throughput.sampledBytes) / elapsed.Seconds()
			if instant > 0 || active[sourceInterface] > 0 {
				if throughput.measured {
					throughput.rate = (throughput.rate + instant) / 2
				} else {
					throughput.rate = instant
					throughput.measured = true
				}
			}
			throughput.sampledBytes = throughput.bytes
			throughput.sampledAt = now
		}
		if !throughput.measured {
			return nil, false
		}
		if throughput.rate <= 0 {
			finishTimes[sourceInterface] = float64(1 << 62)
		} else {
			finishTimes[sourceInterface] = float64(remaining[sourceInterface]) / throughput.rate
		}
	}
	return finishTimes, true
}

func (dp *DownloadProgress) addStolenRangeLocked(start, end int64, sourceInterface string) {
	now := time.Now()
	dp.ensureSourceLocked(sourceInterface, now)
	dp.ranges[start] = &DownloadRange{
		start:           start,
		end:             end,
		current:         start,
		sourceInterface: sourceInterface,
		lastProgressAt:  now,
		sampledAt:       now,
		sampledCurrent:  start,
	}
	dp.notifyLocked()
}

// stealSlowRange uses a smaller split threshold for a connection that has
// stopped making progress or remained below the low-speed threshold long
// enough. This lets an idle worker open a fresh connection for the tail.
func (dp *DownloadProgress) stealSlowRange(now time.Time) (start, end int64, ok bool) {
	start, end, _, ok = dp.stealSlowRangeForInterfaces(now, nil)
	return
}

func (dp *DownloadProgress) stealSlowRangeForInterfaces(now time.Time, interfaces []string) (start, end int64, sourceInterface string, ok bool) {
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
		return 0, 0, "", false
	}

	end = candidate.end
	start = candidate.current + (candidate.end-candidate.current)/2
	sourceInterface = dp.selectLeastBusyInterfaceLocked(interfaces, candidate.sourceInterface)
	if len(dp.sourceLimits) > 0 && sourceInterface == "" {
		return 0, 0, "", false
	}
	dp.addStolenRangeLocked(start, end, sourceInterface)
	candidate.end = start
	return start, end, sourceInterface, true
}

func (dp *DownloadProgress) rangeState() (bool, <-chan struct{}) {
	dp.Lock()
	defer dp.Unlock()
	return len(dp.ranges) > 0 || dp.pendingStart < dp.pendingEnd, dp.changed
}

// waitAndSteal keeps an otherwise idle worker available while other workers
// still own ranges. It first uses normal work stealing, then periodically
// checks whether a stalled or slow connection should be split more eagerly.
func (dp *DownloadProgress) waitAndSteal(ctx context.Context) (DownloadRange, bool) {
	return dp.waitAndStealForInterfaces(ctx, nil)
}

func (dp *DownloadProgress) waitAndStealForInterfaces(ctx context.Context, interfaces []string) (DownloadRange, bool) {
	for {
		if work, ok := dp.takePendingRangeForInterfaces(interfaces); ok {
			return work, true
		}
		if start, end, sourceInterface, ok := dp.stealRangeForInterfaces(interfaces); ok {
			return DownloadRange{start: start, end: end, current: start, sourceInterface: sourceInterface}, true
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
			if start, end, sourceInterface, ok := dp.stealSlowRangeForInterfaces(now, interfaces); ok {
				return DownloadRange{start: start, end: end, current: start, sourceInterface: sourceInterface}, true
			}
		}
	}
}

func downloadFileRequestAt(ctx context.Context, uri string, min, max int64, isHTTP3 bool, file *os.File, progress *DownloadProgress, output chan<- DownloadBlock) error {
	return downloadFileRequestAtOnInterface(ctx, uri, min, max, isHTTP3, interfaceName, file, progress, output)
}

func downloadFileRequestAtOnInterface(ctx context.Context, uri string, min, max int64, isHTTP3 bool, sourceInterface string, file *os.File, progress *DownloadProgress, output chan<- DownloadBlock) error {
	buf := make([]byte, readBufSize)
	client := getHTTPClientForInterface(isHTTP3, sourceInterface)
	if closer, ok := client.Transport.(interface{ Close() error }); ok {
		defer closer.Close()
	}
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

		resp, err := client.Do(req)
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

		if statusErr := checkResponseStatus(resp); statusErr != nil {
			resp.Body.Close()
			stopRequest()
			return statusErr
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
		err := downloadFileRequestAtOnInterface(ctx, uri, start, end, isHTTP3, work.sourceInterface, file, progress, output)
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
		newWork, ok := progress.waitAndStealForInterfaces(ctx, downloadInterfaces())
		if !ok {
			return nil
		}
		work = newWork
	}
}

func configuredSourceWorkers(interfaces []string, maxWorkers int) ([]int, error) {
	if len(interfaceWorkers) == 0 {
		return nil, nil
	}
	if len(interfaceWorkers) != len(interfaces) {
		return nil, fmt.Errorf("--interface-workers needs one count per --interfaces entry")
	}
	counts := make([]int, len(interfaceWorkers))
	seen := make(map[string]bool, len(interfaces))
	total := 0
	for i, count := range interfaceWorkers {
		if seen[interfaces[i]] {
			return nil, fmt.Errorf("--interfaces contains duplicate entry %q", interfaces[i])
		}
		seen[interfaces[i]] = true
		if count < 1 || count > maxWorkers-total {
			return nil, fmt.Errorf("--interface-workers counts must be positive and total no greater than -x")
		}
		counts[i] = count
		total += count
	}
	return counts, nil
}

func initialWorkerSources(interfaces []string, counts []int, workerCount int) []string {
	if counts == nil {
		return nil
	}
	remaining := append([]int(nil), counts...)
	sources := make([]string, 0, workerCount)
	for len(sources) < workerCount {
		for i, sourceInterface := range interfaces {
			if remaining[i] > 0 {
				sources = append(sources, sourceInterface)
				remaining[i]--
				if len(sources) == workerCount {
					break
				}
			}
		}
	}
	return sources
}

func probeInterfaceRate(uri string, isHTTP3 bool, sourceInterface string, connections int, chunkSize int64) (float64, error) {
	started := time.Now()
	errors := make(chan error, connections)
	for i := 0; i < connections; i++ {
		go func(start int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
			if err != nil {
				errors <- err
				return
			}
			SetRequestHeader(req)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+chunkSize-1))
			client := getHTTPClientForInterface(isHTTP3, sourceInterface)
			if closer, ok := client.Transport.(interface{ Close() error }); ok {
				defer closer.Close()
			}
			resp, err := client.Do(req)
			if err != nil {
				errors <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				errors <- fmt.Errorf("probe on %s returned %s", sourceInterface, resp.Status)
				return
			}
			_, err = io.CopyN(io.Discard, resp.Body, chunkSize)
			errors <- err
		}(int64(i) * chunkSize)
	}
	var firstErr error
	for i := 0; i < connections; i++ {
		if err := <-errors; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return float64(int64(connections)*chunkSize) / time.Since(started).Seconds(), nil
}

func chooseAutomaticSourceWorkers(rates [2]float64, ready [2]bool, workers int) ([]int, int, int) {
	if ready[0] != ready[1] {
		counts := []int{0, 0}
		if ready[0] {
			counts[0] = workers
		} else {
			counts[1] = workers
		}
		return counts, -1, -1
	}
	if !ready[0] {
		return nil, -1, -1
	}
	fast, slow := 0, 1
	if rates[1] > rates[0] {
		fast, slow = 1, 0
	}
	if rates[fast] < rates[slow]*1.5 {
		return nil, -1, -1
	}
	counts := []int{1, 1}
	counts[fast] = workers - 1
	return counts, fast, slow
}

type interfaceWorkerTuner struct {
	progress   *DownloadProgress
	interfaces []string
	fast       int
	slow       int
	counts     []int
	lastAt     time.Time
	lastBytes  int64
	baseline   float64
	phase      int
}

func (t *interfaceWorkerTuner) observe(now time.Time, totalBytes int64) {
	if t.phase == 3 {
		return
	}
	elapsed := now.Sub(t.lastAt).Seconds()
	if elapsed <= 0 {
		return
	}
	rate := float64(totalBytes-t.lastBytes) / elapsed
	t.lastAt, t.lastBytes = now, totalBytes
	switch t.phase {
	case 0:
		t.phase = 1
	case 1:
		if rate <= 0 || t.counts[t.fast] <= 1 {
			t.phase = 3
			return
		}
		t.baseline = rate
		t.counts[t.slow]++
		t.counts[t.fast]--
		t.progress.setSourceLimits(t.interfaces, t.counts)
		logStdout.Printf("auto worker trial: %s=%d, %s=%d (baseline %.1f MB/s)", t.interfaces[t.slow], t.counts[t.slow], t.interfaces[t.fast], t.counts[t.fast], rate/1_000_000)
		t.phase = 2
	case 2:
		if rate <= t.baseline*1.03 {
			t.counts[t.slow]--
			t.counts[t.fast]++
			t.progress.setSourceLimits(t.interfaces, t.counts)
			logStdout.Printf("auto worker trial reverted: %s=%d, %s=%d (trial %.1f MB/s)", t.interfaces[t.slow], t.counts[t.slow], t.interfaces[t.fast], t.counts[t.fast], rate/1_000_000)
			t.phase = 3
			return
		}
		t.baseline = rate
		if t.counts[t.slow] >= (t.counts[t.fast]+t.counts[t.slow])/2 || t.counts[t.fast] <= 1 {
			logStdout.Printf("auto worker trial kept: %s=%d, %s=%d (%.1f MB/s)", t.interfaces[t.slow], t.counts[t.slow], t.interfaces[t.fast], t.counts[t.fast], rate/1_000_000)
			t.phase = 3
			return
		}
		logStdout.Printf("auto worker trial kept: %s=%d, %s=%d (%.1f MB/s)", t.interfaces[t.slow], t.counts[t.slow], t.interfaces[t.fast], t.counts[t.fast], rate/1_000_000)
		t.counts[t.slow]++
		t.counts[t.fast]--
		t.progress.setSourceLimits(t.interfaces, t.counts)
		logStdout.Printf("auto worker trial: %s=%d, %s=%d", t.interfaces[t.slow], t.counts[t.slow], t.interfaces[t.fast], t.counts[t.fast])
	}
}

func downloadFileRequest(uri string, contentLength int64, filePath string, isHTTP3 bool) error {
	tsBegin := time.Now()
	if concurrentThread < 1 {
		return fmt.Errorf("download thread count must be positive")
	}
	downloadSources := downloadInterfaces()
	sourceCounts, err := configuredSourceWorkers(downloadSources, concurrentThread)
	if err != nil {
		return err
	}
	if sourceCounts != nil && contentLength <= 0 {
		return fmt.Errorf("--interface-workers requires a known file size for range downloads")
	}
	workerCount := concurrentThread
	if sourceCounts != nil {
		workerCount = 0
		for _, count := range sourceCounts {
			workerCount += count
		}
	}
	if contentLength > 0 && int64(workerCount) > contentLength {
		workerCount = int(contentLength)
	}
	if contentLength <= 0 {
		workerCount = 1
	}
	autoFast, autoSlow := -1, -1
	if sourceCounts == nil && autoInterfaceWorkers && reuseThread && len(downloadSources) == 2 && downloadSources[0] != downloadSources[1] && workerCount >= 4 && contentLength >= 512*1024*1024 {
		probeWorkers := workerCount / 2
		if probeWorkers > 12 {
			probeWorkers = 12
		}
		var rates [2]float64
		var probeOK [2]bool
		for i, sourceInterface := range downloadSources {
			rate, probeErr := probeInterfaceRate(uri, isHTTP3, sourceInterface, probeWorkers, 4*1024*1024)
			if probeErr != nil {
				logStderr.Printf("interface %s probe failed: %v", sourceInterface, probeErr)
				continue
			}
			rates[i], probeOK[i] = rate, true
			logStdout.Printf("interface %s probe: %.1f MB/s", sourceInterface, rate/1_000_000)
		}
		sourceCounts, autoFast, autoSlow = chooseAutomaticSourceWorkers(rates, probeOK, workerCount)
		if sourceCounts != nil {
			logStdout.Printf("auto interface workers: %s=%d, %s=%d", downloadSources[0], sourceCounts[0], downloadSources[1], sourceCounts[1])
		}
		logStdout.Printf("interface probes took %v", time.Since(tsBegin))
		tsBegin = time.Now()
	}
	assignedSources := initialWorkerSources(downloadSources, sourceCounts, workerCount)

	dir := filepath.Dir(filePath)
	_, err = os.Stat(dir)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(chan DownloadBlock)
	done := make(chan error, workerCount)

	var progress *DownloadProgress
	if contentLength > 0 {
		progress = NewDownloadProgress(leastTryBufferSize)
		if sourceCounts != nil {
			progress.setSourceLimits(downloadSources, sourceCounts)
		}
		lenSub := contentLength / int64(workerCount)
		diff := contentLength % int64(workerCount)
		initialRangeSize := lenSub
		if reuseThread && len(downloadSources) > 1 && initialRangeSize > initialDownloadChunkSize {
			initialRangeSize = initialDownloadChunkSize
			progress.setPendingRange(initialRangeSize*int64(workerCount), contentLength, initialRangeSize)
		}
		works := make([]DownloadRange, workerCount)
		for i := 0; i < workerCount; i++ {
			min := initialRangeSize * int64(i)
			max := min + initialRangeSize
			if i == workerCount-1 && initialRangeSize == lenSub {
				max += diff
			}
			sourceInterface := downloadInterfaceForWorker(downloadSources, i)
			if assignedSources != nil {
				sourceInterface = assignedSources[i]
			}
			progress.addRangeOnInterface(min, max, sourceInterface)
			works[i] = DownloadRange{start: min, end: max, current: min, sourceInterface: sourceInterface}
		}
		for _, work := range works {
			go func() {
				done <- downloadWorker(ctx, uri, work, isHTTP3, file, progress, output, reuseThread)
			}()
		}
	} else {
		work := DownloadRange{start: 0, end: -1, current: 0, sourceInterface: downloadInterfaceForWorker(downloadSources, 0)}
		go func() {
			done <- downloadWorker(ctx, uri, work, isHTTP3, file, nil, output, false)
		}()
	}

	var totalReceived int64
	var firstErr error
	var lastProgressPrint time.Time
	var tuneTicker *time.Ticker
	var tuneC <-chan time.Time
	var tuner *interfaceWorkerTuner
	if autoFast >= 0 && autoSlow >= 0 {
		tuner = &interfaceWorkerTuner{progress: progress, interfaces: downloadSources, fast: autoFast, slow: autoSlow, counts: append([]int(nil), sourceCounts...), lastAt: time.Now()}
		tuneTicker = time.NewTicker(10 * time.Second)
		tuneC = tuneTicker.C
		defer tuneTicker.Stop()
	}
	for completed := 0; completed < workerCount; {
		select {
		case now := <-tuneC:
			tuner.observe(now, totalReceived)
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
			if time.Since(lastProgressPrint) >= 200*time.Millisecond {
				lastProgressPrint = time.Now()
				tsCost := time.Since(tsBegin)
				elapsedMilliseconds := tsCost.Milliseconds()
				if elapsedMilliseconds < 1 {
					elapsedMilliseconds = 1
				}
				speed := totalReceived * 1000 / elapsedMilliseconds
				englishPrinter.Printf("\rreceived and wrote %d/%d bytes to offset %d in %+v at %d B/s", totalReceived, contentLength, b.offset, tsCost, speed)
			}
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
