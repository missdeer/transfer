package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

type reverseProxyServeHandler func(*http.ServeMux) error

func createReverseProxy(h reverseProxyServeHandler, target string, wg *sync.WaitGroup) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		url, err := url.Parse(target)
		if err != nil {
			log.Println(target, err)
			return
		}

		proxy := httputil.NewSingleHostReverseProxy(url)
		proxy.ServeHTTP(w, r)
	})
	log.Fatal(h(mux))
	wg.Done()
}
