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
	u, err := url.Parse(target)
	if err != nil {
		logStderr.Println(target, err)
		wg.Done()
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	if interfaceName != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = dialOutbound
		proxy.Transport = transport
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})
	logStderr.Fatal(h(mux))
	wg.Done()
}
