package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadProgressStealsLargestRemainingRange(t *testing.T) {
	progress := NewDownloadProgress(20)
	progress.addRange(0, 200)
	progress.addRange(200, 300)
	progress.updateRange(0, 40)
	progress.updateRange(200, 250)

	start, end, ok := progress.stealRange()
	if !ok {
		t.Fatal("expected work to be stolen")
	}
	if start != 120 || end != 200 {
		t.Fatalf("stolen range = [%d, %d), want [120, 200)", start, end)
	}

	ownerEnd, ok := progress.updateRange(0, 80)
	if !ok {
		t.Fatal("owner range disappeared")
	}
	if ownerEnd != start {
		t.Fatalf("owner end = %d, want stolen start %d", ownerEnd, start)
	}
}

func TestDownloadInterfaceForWorkerRoundRobin(t *testing.T) {
	interfaces := []string{"left", "right"}
	for worker, want := range []string{"left", "right", "left", "right"} {
		if got := downloadInterfaceForWorker(interfaces, worker); got != want {
			t.Fatalf("worker %d interface = %q, want %q", worker, got, want)
		}
	}
	if got := downloadInterfaceForWorker(nil, 0); got != "" {
		t.Fatalf("empty interface list selected %q", got)
	}
}

func TestConfiguredSourceWorkers(t *testing.T) {
	previous := interfaceWorkers
	interfaceWorkers = []int{1, 12}
	defer func() { interfaceWorkers = previous }()

	interfaces := []string{"wired", "wifi"}
	counts, err := configuredSourceWorkers(interfaces, 24)
	if err != nil {
		t.Fatal(err)
	}
	sources := initialWorkerSources(interfaces, counts, 13)
	wired, wifi := 0, 0
	for _, source := range sources {
		if source == "wired" {
			wired++
		} else if source == "wifi" {
			wifi++
		}
	}
	if wired != 1 || wifi != 12 {
		t.Fatalf("initial workers wired=%d wifi=%d, want 1 and 12", wired, wifi)
	}
	if _, err := configuredSourceWorkers(interfaces, 12); err == nil {
		t.Fatal("expected counts above -x to fail")
	}
	if _, err := configuredSourceWorkers([]string{"wired"}, 24); err == nil {
		t.Fatal("expected source/count length mismatch to fail")
	}
	if _, err := configuredSourceWorkers([]string{"wired", "wired"}, 24); err == nil {
		t.Fatal("expected duplicate interfaces to fail")
	}
}

func TestPendingWorkHonorsInterfaceWorkerLimits(t *testing.T) {
	progress := NewDownloadProgress(2)
	progress.setSourceLimits([]string{"wired", "wifi"}, []int{1, 2})
	progress.addRangeOnInterface(0, 100, "wired")
	progress.addRangeOnInterface(100, 200, "wifi")
	progress.addRangeOnInterface(200, 300, "wifi")
	progress.setPendingRange(300, 500, 100)

	if work, ok := progress.takePendingRangeForInterfaces([]string{"wired", "wifi"}); ok {
		t.Fatalf("assigned %q while both interfaces were at their limits", work.sourceInterface)
	}
	progress.removeRange(100)
	work, ok := progress.takePendingRangeForInterfaces([]string{"wired", "wifi"})
	if !ok || work.sourceInterface != "wifi" || work.start != 300 {
		t.Fatalf("first pending range = %+v, ok=%t; want wifi at 300", work, ok)
	}
	progress.removeRange(0)
	work, ok = progress.takePendingRangeForInterfaces([]string{"wired", "wifi"})
	if !ok || work.sourceInterface != "wired" || work.start != 400 {
		t.Fatalf("second pending range = %+v, ok=%t; want wired at 400", work, ok)
	}
}

func TestProbeInterfaceRate(t *testing.T) {
	data := bytes.Repeat([]byte("probe"), 2048)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	rate, err := probeInterfaceRate(server.URL, false, "127.0.0.1", 2, 1024)
	if err != nil || rate <= 0 || requests.Load() != 2 {
		t.Fatalf("probe rate=%f, requests=%d, error=%v", rate, requests.Load(), err)
	}
}

func TestChooseAutomaticSourceWorkers(t *testing.T) {
	counts, fast, slow := chooseAutomaticSourceWorkers([2]float64{10, 30}, [2]bool{true, true}, 24)
	if !slices.Equal(counts, []int{1, 23}) || fast != 1 || slow != 0 {
		t.Fatalf("unequal routes: counts=%v, fast=%d slow=%d", counts, fast, slow)
	}
	counts, fast, slow = chooseAutomaticSourceWorkers([2]float64{30, 28}, [2]bool{true, true}, 24)
	if counts != nil || fast != -1 || slow != -1 {
		t.Fatalf("similar routes should retain round-robin: counts=%v, fast=%d slow=%d", counts, fast, slow)
	}
	counts, _, _ = chooseAutomaticSourceWorkers([2]float64{30, 0}, [2]bool{true, false}, 24)
	if !slices.Equal(counts, []int{24, 0}) {
		t.Fatalf("unavailable route: counts=%v, want [24 0]", counts)
	}
}

func TestInterfaceWorkerTunerRevertsUnhelpfulTrial(t *testing.T) {
	progress := NewDownloadProgress(2)
	interfaces := []string{"wired", "wifi"}
	progress.setSourceLimits(interfaces, []int{1, 23})
	started := time.Now()
	tuner := &interfaceWorkerTuner{progress: progress, interfaces: interfaces, fast: 1, slow: 0, counts: []int{1, 23}, lastAt: started}
	tuner.observe(started.Add(10*time.Second), 1000)
	tuner.observe(started.Add(20*time.Second), 2000)
	progress.Lock()
	trialCount := progress.sourceLimits["wired"]
	progress.Unlock()
	if trialCount != 2 {
		t.Fatalf("trial wired limit = %d, want 2", trialCount)
	}
	tuner.observe(started.Add(30*time.Second), 2800)
	progress.Lock()
	wired, wifi := progress.sourceLimits["wired"], progress.sourceLimits["wifi"]
	progress.Unlock()
	if wired != 1 || wifi != 23 || tuner.phase != 3 {
		t.Fatalf("reverted limits wired=%d wifi=%d phase=%d, want 1, 23, 3", wired, wifi, tuner.phase)
	}
}

func TestInterfaceWorkerTunerKeepsHelpfulTrial(t *testing.T) {
	progress := NewDownloadProgress(2)
	interfaces := []string{"wired", "wifi"}
	progress.setSourceLimits(interfaces, []int{1, 23})
	started := time.Now()
	tuner := &interfaceWorkerTuner{progress: progress, interfaces: interfaces, fast: 1, slow: 0, counts: []int{1, 23}, lastAt: started}
	tuner.observe(started.Add(10*time.Second), 1000)
	tuner.observe(started.Add(20*time.Second), 2000)
	tuner.observe(started.Add(30*time.Second), 3200)
	progress.Lock()
	wired, wifi := progress.sourceLimits["wired"], progress.sourceLimits["wifi"]
	progress.Unlock()
	if wired != 3 || wifi != 21 || tuner.phase != 2 {
		t.Fatalf("accepted trial limits wired=%d wifi=%d phase=%d, want 3, 21, 2", wired, wifi, tuner.phase)
	}
}

func TestDownloadProgressStealsToLeastBusyInterface(t *testing.T) {
	progress := NewDownloadProgress(2)
	progress.addRangeOnInterface(0, 200, "left")
	progress.addRangeOnInterface(200, 250, "left")
	progress.addRangeOnInterface(250, 300, "right")

	start, end, source, ok := progress.stealRangeForInterfaces([]string{"left", "right"})
	if !ok {
		t.Fatal("expected work to be stolen")
	}
	if start != 100 || end != 200 || source != "right" {
		t.Fatalf("stolen range = [%d, %d) on %q, want [100, 200) on right", start, end, source)
	}
}

func TestDownloadProgressMeasuresInterfaceThroughput(t *testing.T) {
	progress := NewDownloadProgress(2)
	progress.addRangeOnInterface(0, 100, "wired")
	progress.addRangeOnInterface(100, 200, "wifi")
	progress.Lock()
	for _, source := range progress.sources {
		source.sampledAt = time.Now().Add(-2 * time.Second)
	}
	progress.Unlock()
	progress.updateRange(0, 10)
	progress.updateRange(100, 130)

	progress.Lock()
	_, measured := progress.estimatedFinishTimesLocked([]string{"wired", "wifi"}, time.Now())
	wiredRate := progress.sources["wired"].rate
	wifiRate := progress.sources["wifi"].rate
	progress.Unlock()
	if !measured || wifiRate < wiredRate*2.5 {
		t.Fatalf("measured rates wired=%f wifi=%f, want wifi about 3x wired", wiredRate, wifiRate)
	}
}

func TestDownloadProgressMovesWorkTowardFasterInterface(t *testing.T) {
	progress := NewDownloadProgress(2)
	for i := int64(0); i < 8; i++ {
		progress.addRangeOnInterface(i*100, (i+1)*100, "wired")
		progress.addRangeOnInterface(800+i*100, 800+(i+1)*100, "wifi")
	}
	progress.Lock()
	progress.sources["wired"].rate = 10
	progress.sources["wired"].measured = true
	progress.sources["wifi"].rate = 30
	progress.sources["wifi"].measured = true
	progress.Unlock()

	for i := 0; i < 4; i++ {
		_, _, source, ok := progress.stealRangeForInterfaces([]string{"wired", "wifi"})
		if !ok || source != "wifi" {
			t.Fatalf("steal %d assigned to %q, want wifi", i, source)
		}
	}
	progress.Lock()
	defer progress.Unlock()
	wiredRanges, wifiRanges := 0, 0
	for _, r := range progress.ranges {
		if r.sourceInterface == "wired" {
			wiredRanges++
		} else {
			wifiRanges++
		}
	}
	if wiredRanges != 8 || wifiRanges != 12 {
		t.Fatalf("active ranges wired=%d wifi=%d, want 8 and 12", wiredRanges, wifiRanges)
	}
}

func TestDownloadProgressKeepsBalancedWorkOnSameInterface(t *testing.T) {
	progress := NewDownloadProgress(2)
	progress.addRangeOnInterface(0, 100, "wired")
	progress.addRangeOnInterface(100, 400, "wifi")
	progress.Lock()
	progress.sources["wired"].rate = 10
	progress.sources["wired"].measured = true
	progress.sources["wifi"].rate = 30
	progress.sources["wifi"].measured = true
	progress.Unlock()

	_, _, source, ok := progress.stealRangeForInterfaces([]string{"wired", "wifi"})
	if !ok || source != "wifi" {
		t.Fatalf("balanced steal assigned to %q, want wifi", source)
	}
}

func TestDownloadProgressSplitsByMeasuredCapacity(t *testing.T) {
	progress := NewDownloadProgress(2)
	progress.addRangeOnInterface(0, 1000, "wired")
	progress.addRangeOnInterface(1000, 1001, "wifi")
	progress.removeRange(1000)
	progress.Lock()
	progress.sources["wired"].rate = 10
	progress.sources["wired"].measured = true
	progress.sources["wifi"].rate = 30
	progress.sources["wifi"].measured = true
	progress.Unlock()

	start, end, source, ok := progress.stealRangeForInterfaces([]string{"wired", "wifi"})
	if !ok || start != 250 || end != 1000 || source != "wifi" {
		t.Fatalf("stolen range = [%d, %d) on %q, want [250, 1000) on wifi", start, end, source)
	}
}

func TestDownloadProgressDoesNotStealSmallRange(t *testing.T) {
	progress := NewDownloadProgress(20)
	progress.addRange(0, 100)
	progress.updateRange(0, 81)

	if _, _, ok := progress.stealRange(); ok {
		t.Fatal("unexpectedly stole a range smaller than the minimum")
	}
}

func TestDownloadProgressStealsStalledRangeBelowNormalThreshold(t *testing.T) {
	progress := NewDownloadProgress(1_000)
	progress.minSlowSteal = 2
	progress.stallTimeout = time.Second
	progress.addRange(0, 100)
	progress.Lock()
	progress.ranges[0].lastProgressAt = time.Now().Add(-2 * time.Second)
	progress.Unlock()

	start, end, ok := progress.stealSlowRange(time.Now())
	if !ok {
		t.Fatal("expected stalled work to be stolen")
	}
	if start != 50 || end != 100 {
		t.Fatalf("stolen range = [%d, %d), want [50, 100)", start, end)
	}
}

func TestDownloadProgressStealsPersistentlySlowRange(t *testing.T) {
	progress := NewDownloadProgress(1_000)
	progress.minSlowSteal = 2
	progress.stallTimeout = time.Hour
	progress.slowWindow = time.Second
	progress.lowSpeed = 1_000
	progress.addRange(0, 100)
	progress.Lock()
	progress.ranges[0].sampledAt = time.Now().Add(-2 * time.Second)
	progress.ranges[0].lastProgressAt = time.Now()
	progress.Unlock()
	progress.updateRange(0, 10)

	start, end, ok := progress.stealSlowRange(time.Now())
	if !ok {
		t.Fatal("expected slow work to be stolen")
	}
	if start != 55 || end != 100 {
		t.Fatalf("stolen range = [%d, %d), want [55, 100)", start, end)
	}
}

func TestDownloadReconnectsAfterNoTraffic(t *testing.T) {
	data := []byte("download resumed on a fresh connection")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
				return
			}
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()

	oldConcurrentThread := concurrentThread
	oldReadBufSize := readBufSize
	oldRetryTimes := retryTimes
	oldStallTimeout := workStealStallTimeout
	defer func() {
		concurrentThread = oldConcurrentThread
		readBufSize = oldReadBufSize
		retryTimes = oldRetryTimes
		workStealStallTimeout = oldStallTimeout
	}()
	concurrentThread = 1
	readBufSize = 4 * 1024
	retryTimes = 3
	workStealStallTimeout = 20 * time.Millisecond

	outputPath := filepath.Join(t.TempDir(), "reconnected.bin")
	if err := downloadFileRequest(server.URL, 0, outputPath, false); err != nil {
		t.Fatal(err)
	}
	downloaded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, data) {
		t.Fatal("downloaded content does not match source")
	}
	if requests.Load() < 2 {
		t.Fatalf("request count = %d, want at least 2", requests.Load())
	}
}

func TestDownloadWorkersStealSlowRange(t *testing.T) {
	data := bytes.Repeat([]byte("work-stealing-download-"), 8192)
	half := int64(len(data) / 2)

	var mu sync.Mutex
	var requestedStarts []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requestedStarts = append(requestedStarts, start)
		mu.Unlock()

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if start != half {
			_, _ = w.Write(data[start : end+1])
			return
		}

		for offset := start; offset <= end; offset += 1024 {
			chunkEnd := offset + 1024
			if chunkEnd > end+1 {
				chunkEnd = end + 1
			}
			if _, err := w.Write(data[offset:chunkEnd]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(time.Millisecond)
		}
	}))
	defer server.Close()

	oldConcurrentThread := concurrentThread
	oldReadBufSize := readBufSize
	oldLeastTryBufferSize := leastTryBufferSize
	oldRetryTimes := retryTimes
	oldReuseThread := reuseThread
	defer func() {
		concurrentThread = oldConcurrentThread
		readBufSize = oldReadBufSize
		leastTryBufferSize = oldLeastTryBufferSize
		retryTimes = oldRetryTimes
		reuseThread = oldReuseThread
	}()
	concurrentThread = 2
	readBufSize = 4 * 1024
	leastTryBufferSize = 16 * 1024
	retryTimes = 2
	reuseThread = true

	outputPath := filepath.Join(t.TempDir(), "download.bin")
	if err := downloadFileRequest(server.URL, int64(len(data)), outputPath, false); err != nil {
		t.Fatal(err)
	}
	downloaded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, data) {
		t.Fatal("downloaded content does not match source")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, start := range requestedStarts {
		if start > half {
			return
		}
	}
	t.Fatalf("no worker stole the slow range; request starts: %v", requestedStarts)
}

func TestDownloadWorkersDrainPendingRanges(t *testing.T) {
	data := bytes.Repeat([]byte("pending-range-download"), 8192)
	var pendingRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}
		if start >= 32*1024 {
			pendingRequests.Add(1)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	oldThreads, oldBuffer, oldStealSize := concurrentThread, readBufSize, leastTryBufferSize
	oldRetry, oldReuse := retryTimes, reuseThread
	oldInterfaces, oldChunkSize := interfaceNames, initialDownloadChunkSize
	defer func() {
		concurrentThread, readBufSize, leastTryBufferSize = oldThreads, oldBuffer, oldStealSize
		retryTimes, reuseThread = oldRetry, oldReuse
		interfaceNames, initialDownloadChunkSize = oldInterfaces, oldChunkSize
	}()
	concurrentThread, readBufSize, leastTryBufferSize = 2, 4*1024, 16*1024
	retryTimes, reuseThread = 2, true
	interfaceNames = []string{"127.0.0.1", "127.0.0.1"}
	initialDownloadChunkSize = 16 * 1024

	outputPath := filepath.Join(t.TempDir(), "pending.bin")
	if err := downloadFileRequest(server.URL, int64(len(data)), outputPath, false); err != nil {
		t.Fatal(err)
	}
	downloaded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, data) || pendingRequests.Load() == 0 {
		t.Fatalf("pending ranges not fully downloaded, pending requests = %d", pendingRequests.Load())
	}
}
