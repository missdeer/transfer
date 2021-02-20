package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lucas-clemente/quic-go/http3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var (
	englishPrinter = message.NewPrinter(language.English)
	regHTTP3AltSvc = regexp.MustCompile(`h3\-(29|32)="(.*)";\s*ma=[0-9]+`)
)

// Creates a new file upload http request with optional extra params
func newfileUploadRequest(uri string, params map[string]string, paramName, filePath string) (*http.Request, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(paramName, filepath.Base(filePath))
	if err != nil {
		return nil, 0, err
	}
	length, err := io.Copy(part, file)
	if err != nil {
		return nil, 0, err
	}

	for key, val := range params {
		_ = writer.WriteField(key, val)
	}
	err = writer.Close()
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest("POST", uri, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, length, err
}
func getHTTPClient(isHTTP3 bool) *http.Client {
	if isHTTP3 {
		return &http.Client{
			Transport: &http3.RoundTripper{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: insecureSkipVerify,
				},
			},
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecureSkipVerify,
			},
			Dial: func(network, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout(network, addr, time.Second*30)
				if err != nil {
					return conn, err
				}

				tcpConn := conn.(*net.TCPConn)
				tcpConn.SetKeepAlive(false)

				return tcpConn, err
			},
			DisableKeepAlives: true,
		},
	}
}

func uploadFileRequest(uri string, filePath string, isHTTP3 bool) error {
	extraParams := map[string]string{
		"title":       filepath.Base(filePath),
		"author":      "CUBE SA",
		"description": fmt.Sprintf("file %s uploaded by CUBE SA", filepath.Base(filePath)),
	}
	request, totalSent, err := newfileUploadRequest(uri, extraParams, uploadFormFileName, filePath)
	if err != nil {
		log.Println(err)
		return err
	}
	client := getHTTPClient(isHTTP3)
	tsBegin := time.Now()
	resp, err := client.Do(request)
	if err != nil {
		log.Println(err)
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
		return err
	}
	tsEnd := time.Now()
	tsCost := tsEnd.Sub(tsBegin)
	speed := totalSent * 1000 / int64(tsCost/time.Millisecond)
	englishPrinter.Printf("\rsent %d bytes in %+v at %d B/s, received response: %s\n", totalSent, tsCost, speed, string(body))
	return nil
}

// DownloadBlock defines download content
type DownloadBlock struct {
	offset int64
	length int64
	buf    []byte
}

func downloadFileRequestAt(ctx context.Context, uri string, min int64, max int64, isHTTP3 bool, output chan DownloadBlock, done chan error) error {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		done <- err
		return err
	}
	client := getHTTPClient(isHTTP3)

	rangeHeader := fmt.Sprintf("bytes=%d-%d", min, max-1) // Add the data for the Range header of the form "bytes=0-100"
	req.Header.Add("Range", rangeHeader)
	resp, err := client.Do(req)
	if err != nil {
		done <- err
		return err
	}
	defer resp.Body.Close()

	offset := min
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			goto exit
		default:
			nr, er := resp.Body.Read(buf)
			if nr > 0 {
				output <- DownloadBlock{
					offset: offset,
					length: int64(nr),
					buf:    buf[0:nr],
				}
				offset += int64(nr)
			}
			if er != nil {
				if er != io.EOF {
					err = er
				}
				goto exit
			}
		}
	}
exit:
	fmt.Printf("\nend a thread from %d to %d, total received bytes: %d\n", min, max, offset-min)
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

func getHTTPResponseHeader(uri string) (http.Header, error) {
	req, err := http.NewRequest("HEAD", uri, nil)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	client := getHTTPClient(false)
	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	defer resp.Body.Close()

	return resp.Header, nil
}

func getContentLength(headers http.Header) (int64, error) {
	contentLength := headers.Get("Content-Length")
	expectedLength, err := strconv.ParseInt(contentLength, 10, 64)
	if err != nil {
		log.Println("parsing content-length", err)
	}
	return expectedLength, err
}

func isHTTP3Enabled(uri string, headers http.Header) (string, bool, error) {
	altSvc := headers.Get("alt-svc")
	ss := strings.Split(altSvc, ",")
	for _, s := range ss {
		h3ma := regHTTP3AltSvc.FindAllStringSubmatch(s, -1)
		if len(h3ma) > 0 && len(h3ma[0]) == 3 {
			newPort := h3ma[0][2]
			u, err := url.Parse(uri)
			if err != nil {
				continue
			}
			host := strings.Split(u.Host, ":")
			if len(host) == 2 {
				host[1] = newPort[1:]
				u.Host = strings.Join(host, ":")
			} else {
				u.Host = host[0] + newPort
			}
			u.Scheme = "https"
			return u.String(), true, nil
		}
	}
	return uri, false, nil
}
