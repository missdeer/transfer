package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
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
