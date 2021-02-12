package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lucas-clemente/quic-go/http3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Creates a new file upload http request with optional extra params
func newfileUploadRequest(uri string, params map[string]string, paramName, filePath string) (*http.Request, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(paramName, filepath.Base(filePath))
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(part, file)

	for key, val := range params {
		_ = writer.WriteField(key, val)
	}
	err = writer.Close()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", uri, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, err
}

func uploadFileRequest(uri string, filePath string, isHTTP3 bool) error {
	extraParams := map[string]string{
		"title":       filepath.Base(filePath),
		"author":      "CUBE SA",
		"description": fmt.Sprintf("file %s uploaded by CUBE SA", filepath.Base(filePath)),
	}
	request, err := newfileUploadRequest(uri, extraParams, uploadFormFileName, filePath)
	if err != nil {
		log.Println(err)
		return err
	}
	var client *http.Client
	if isHTTP3 {
		client = &http.Client{Transport: &http3.RoundTripper{}}
	} else {
		client = &http.Client{}
	}
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
	log.Println(string(body))
	return nil
}

func downloadFileRequest(uri string, filePath string, isHTTP3 bool) error {
	tsBegin := time.Now()
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		log.Println(err)
		return err
	}
	var client *http.Client
	if isHTTP3 {
		client = &http.Client{
			Transport: &http3.RoundTripper{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: insecureSkipVerify,
				},
			},
		}
	} else {
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: insecureSkipVerify,
				},
			},
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return err
	}
	defer resp.Body.Close()

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

	englishPrinter := message.NewPrinter(language.English)
	var totalReceived int64
	buf := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := fd.Write(buf[0:nr])
			if nw > 0 {
				totalReceived += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
			englishPrinter.Printf("\rreceived and wrote %d bytes", totalReceived)
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}

	fmt.Printf("\n")
	if err != nil && err != io.EOF {
		log.Println(err)
	} else {
		tsEnd := time.Now()
		tsCost := tsEnd.Sub(tsBegin)
		speed := totalReceived / int64(tsCost/time.Second)
		logs := englishPrinter.Sprintf("%d bytes received and written to %s in %+v at %d B/s\n", totalReceived, filePath, tsCost, speed)
		log.Printf(logs)
	}
	return err
}

func isHTTP3Enabled(uri string) (string, bool, error) {
	if strings.ToLower(protocol) == "quic" {
		return uri, true, nil
	}

	req, err := http.NewRequest("HEAD", uri, nil)
	if err != nil {
		log.Println(err)
		return uri, false, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecureSkipVerify,
			},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return uri, false, err
	}
	defer resp.Body.Close()

	altSvc := resp.Header.Get("alt-svc")
	ss := strings.Split(altSvc, ",")
	r := regexp.MustCompile(`h3\-(29|32)="(.*)";\s*ma=[0-9]+`)
	for _, s := range ss {
		h3ma := r.FindAllStringSubmatch(s, -1)
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
