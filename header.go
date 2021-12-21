package main

import (
	"log"
	"net/http"
)

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
