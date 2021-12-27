package main

import (
	"net/http"

	"github.com/go-httpproxy/httpproxy"
)

func onError(ctx *httpproxy.Context, where string,
	err *httpproxy.Error, opErr error) {
	// Log errors.
	logStderr.Printf("ERR: %s: %s [%s]\n", where, err, opErr)
}

func onAccept(ctx *httpproxy.Context, w http.ResponseWriter,
	r *http.Request) bool {
	// Handle local request has path "/info"
	if r.Method == "GET" && !r.URL.IsAbs() && r.URL.Path == "/info" {
		w.Write([]byte("This is go-httpproxy."))
		return true
	}
	return false
}

func onAuth(ctx *httpproxy.Context, authType string, user string, pass string) bool {
	// not supported yet
	return true
}

func onConnect(ctx *httpproxy.Context, host string) (ConnectAction httpproxy.ConnectAction, newHost string) {
	// Apply "Man in the Middle" to all ssl connections. Never change host.
	return httpproxy.ConnectMitm, host
}

func onRequest(ctx *httpproxy.Context, req *http.Request) (
	resp *http.Response) {
	// Log proxying requests.
	logStdout.Printf("INFO: Proxy: %s %s\n", req.Method, req.URL.String())
	return
}

func onResponse(ctx *httpproxy.Context, req *http.Request, resp *http.Response) {
	// Add header "Via: go-httpproxy".
	resp.Header.Add("Via", "CUBE SA Transfer")
}

func createProxy() *httpproxy.Proxy {
	prx, _ := httpproxy.NewProxy()

	// Set handlers.
	prx.OnError = onError
	prx.OnAccept = onAccept
	prx.OnAuth = onAuth
	prx.OnConnect = onConnect
	prx.OnRequest = onRequest
	prx.OnResponse = onResponse

	return prx
}
