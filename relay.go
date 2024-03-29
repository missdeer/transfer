package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

type reverseProxyServeHandler func(*http.ServeMux) error

func createReverseProxy(h reverseProxyServeHandler, target string, wg *sync.WaitGroup) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u, err := url.Parse(target)
		if err != nil {
			logStderr.Println(target, err)
			return
		}

		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.ServeHTTP(w, r)
	})
	logStderr.Fatal(h(mux))
	wg.Done()
}
