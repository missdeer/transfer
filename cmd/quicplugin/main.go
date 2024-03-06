package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/quic-go/quic-go/http3"
	"github.com/missdeer/transfer/keypair"
	flag "github.com/spf13/pflag"
)

var (
	listenAddr string
	certFile   string
	keyFile    string
)

func printExamples() {
	fmt.Println("Examples:")
	fmt.Println("\tquicplugin")
	fmt.Println("\tquicplugin -k example.com.key -t fullchain.cer")
}

func listenAndServe(certFile, keyFile string, handler http.Handler) error {
	// Load certs
	var err error
	kpr, err := keypair.NewKeypairReloader(certFile, keyFile)
	if err != nil {
		return err
	}
	// We currently only use the cert-related stuff from tls.Config,
	// so we don't need to make a full copy.
	config := &tls.Config{GetCertificate: kpr.GetCertificateFunc()}

	// Open the listeners
	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	defer udpConn.Close()

	// Start the servers
	httpServer := &http.Server{
		Addr:      listenAddr,
		TLSConfig: config,
	}

	quicServer := &http3.Server{
		Addr:      listenAddr,
		TLSConfig: config,
	}

	if handler == nil {
		handler = http.DefaultServeMux
	}

	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	})

	hErr := make(chan error)
	qErr := make(chan error)
	go func() {
		qErr <- quicServer.Serve(udpConn)
	}()

	select {
	case err := <-hErr:
		quicServer.Close()
		return err
	case err := <-qErr:
		// Cannot close the HTTP server or wait for requests to complete properly :/
		return err
	}
}

func main() {
	help := false
	flag.StringVarP(&listenAddr, "listen", "l", "0.0.0.0:443", "listen address, server/proxy mode only")
	flag.StringVarP(&certFile, "cert", "t", "cert.pem", "SSL certificate file path")
	flag.StringVarP(&keyFile, "key", "k", "key.pem", "SSL key file path")
	flag.BoolVarP(&help, "help", "h", false, "show this help message")
	flag.Parse()

	if help {
		printExamples()
		fmt.Printf("\n")
		flag.PrintDefaults()
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u := *r.URL
		u.Host = "127.0.0.1"
		u.Scheme = "http"
		u.Path = "/"
		proxy := httputil.NewSingleHostReverseProxy(&u)
		proxy.ServeHTTP(w, r)
	})

	log.Fatal(listenAndServe(certFile, keyFile, mux))
}
