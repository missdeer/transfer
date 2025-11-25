package main

import (
	"net/http"
)

func getHTTPResponseHeader(uri string) (http.Header, error) {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		logStderr.Println(err)
		return nil, err
	}

	SetRequestHeader(req)
	req.Header.Set("Range", "bytes=0-0")
	client := getHTTPClient(false)
	resp, err := client.Do(req)
	if err != nil {
		logStderr.Println(err)
		return nil, err
	}
	defer resp.Body.Close()

	// Read and discard the response body (only 1 byte)
	_, _ = resp.Body.Read(make([]byte, 1))

	return resp.Header, nil
}
