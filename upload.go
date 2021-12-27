package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
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
	SetRequestHeader(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, length, err
}

func uploadFileRequest(uri string, filePath string, isHTTP3 bool) error {
	extraParams := map[string]string{
		"title":       filepath.Base(filePath),
		"author":      "CUBE SA",
		"description": fmt.Sprintf("file %s uploaded by CUBE SA", filepath.Base(filePath)),
	}
	request, totalSent, err := newfileUploadRequest(uri, extraParams, uploadFormFileName, filePath)
	if err != nil {
		logStderr.Println(err)
		return err
	}
	client := getHTTPClient(isHTTP3)
	tsBegin := time.Now()
	resp, err := client.Do(request)
	if err != nil {
		logStderr.Println(err)
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logStderr.Println(err)
		return err
	}
	tsEnd := time.Now()
	tsCost := tsEnd.Sub(tsBegin)
	speed := totalSent * 1000 / int64(tsCost/time.Millisecond)
	logs := englishPrinter.Sprintf("\rsent %d bytes in %+v at %d B/s, received response: %s\n", totalSent, tsCost, speed, string(body))
	logStdout.Println(logs)
	return nil
}
