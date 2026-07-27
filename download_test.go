package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
