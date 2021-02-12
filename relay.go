package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

func createReverseProxy(listen string, target string, wg *sync.WaitGroup, isHTTP3, quicOnly bool) {
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
	if isHTTP3 {
		log.Fatal(listenAndServe(listen, certFile, keyFile, mux, quicOnly))
	} else {
		s := http.Server{
			Addr:    listen,
			Handler: mux,
		}
		log.Fatal(s.ListenAndServe())
	}
	wg.Done()
}
