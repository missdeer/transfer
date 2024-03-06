package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"
)

var (
	regHTTP3AltSvc = regexp.MustCompile(`h3\-(29|32)="(.*)";\s*ma=[0-9]+`)
)

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
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialer := &net.Dialer{
					Timeout: time.Second * 30,
				}
				conn, err := dialer.DialContext(ctx, network, addr)
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

func getContentLength(headers http.Header) (int64, error) {
	contentLength := headers.Get("Content-Length")
	expectedLength, err := strconv.ParseInt(contentLength, 10, 64)
	if err != nil {
		logStdout.Println("parsing content-length", err)
	}
	return expectedLength, err
}

func needDownload(headers http.Header, contentLength int64, filePath string) bool {
	fi, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		return true
	}

	if fi.Size() != contentLength {
		return true
	}

	lastModified := headers.Get("Last-Modified")
	if lastModified == "" {
		return true
	}

	const layout = "Mon, 02 Jan 2006 15:04:05 MST"
	fileLastModifiedTime, err := time.Parse(layout, lastModified)
	if err != nil {
		logStdout.Println(err)
		return true
	}

	if fileLastModifiedTime.After(fi.ModTime()) {
		return true
	}

	return false
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

func SetRequestHeader(req *http.Request) {
	for _, v := range headers {
		index := strings.Index(v, ":")
		if index == -1 {
			continue
		}
		key := strings.TrimSpace(v[:index])
		value := strings.TrimSpace(v[index+1:])
		req.Header.Set(key, value)
	}
}
