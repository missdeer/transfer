package main

import (
	"net/http"
)

func getHTTPResponseHeader(uri string) (http.Header, error) {
	return getHTTPResponseHeaderAt(uri, interfaceName)
}

func getHTTPResponseHeaderForInterfaces(uri string, sourceInterfaces []string) (http.Header, error) {
	if len(sourceInterfaces) == 0 {
		return getHTTPResponseHeaderAt(uri, "")
	}
	var lastErr error
	for _, sourceInterface := range sourceInterfaces {
		headers, err := getHTTPResponseHeaderAt(uri, sourceInterface)
		if err == nil {
			return headers, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func getHTTPResponseHeaderAt(uri, sourceInterface string) (http.Header, error) {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		logStderr.Println(err)
		return nil, err
	}

	SetRequestHeader(req)
	req.Header.Set("Range", "bytes=0-0")
	client := getHTTPClientForInterface(false, sourceInterface)
	resp, err := client.Do(req)
	if err != nil {
		logStderr.Println(err)
		return nil, err
	}
	defer resp.Body.Close()

	// Read and discard the response body (only 1 byte)
	_, _ = resp.Body.Read(make([]byte, 1))

	if err = checkResponseStatus(resp); err != nil {
		logStderr.Println(err)
		return resp.Header, err
	}

	return resp.Header, nil
}
