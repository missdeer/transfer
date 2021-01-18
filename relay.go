package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

func createReverseProxy(listen string, target string, wg *sync.WaitGroup) {
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
	s := http.Server{
		Addr:    listen,
		Handler: mux,
	}
	log.Fatal(s.ListenAndServe())
	wg.Done()
}
