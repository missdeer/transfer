package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lucas-clemente/quic-go/http3"
)

func uploadFileHandler(w http.ResponseWriter, r *http.Request) {
	// upload size
	err := r.ParseMultipartForm(200000) // grab the multipart form
	if err != nil {
		fmt.Fprintln(w, err)
	}

	// reading original file
	file, handler, err := r.FormFile(uploadFormFileName)
	if err != nil {
		fmt.Println("Error Retrieving the File")
		fmt.Println(err)
		return
	}
	defer file.Close()

	tempFileName := filepath.Join(fileServePath, "."+handler.Filename+"~")
	resFile, err := os.Create(tempFileName)
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}

	_, err = io.Copy(resFile, file)
	resFile.Close()

	if err != nil {
		fmt.Fprintln(w, err)
		return
	}

	destFileName := filepath.Join(fileServePath, handler.Filename)
	os.Rename(tempFileName, destFileName)
	fmt.Fprintf(w, "Successfully Uploaded Original File\n")
}

func listenAndServe(addr, certFile, keyFile string, handler http.Handler) error {
	// Load certs
	var err error
	kpr, err := NewKeypairReloader(certFile, keyFile)
	if err != nil {
		return err
	}
	// We currently only use the cert-related stuff from tls.Config,
	// so we don't need to make a full copy.
	config := &tls.Config{GetCertificate: kpr.GetCertificateFunc()}

	// Open the listeners
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	defer udpConn.Close()

	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return err
	}
	tcpConn, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return err
	}
	defer tcpConn.Close()

	tlsConn := tls.NewListener(tcpConn, config)
	defer tlsConn.Close()

	// Start the servers
	httpServer := &http.Server{
		Addr:      addr,
		TLSConfig: config,
	}

	quicServer := &http3.Server{
		Server: httpServer,
	}

	if handler == nil {
		handler = http.DefaultServeMux
	}
	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quicServer.SetQuicHeaders(w.Header())
		handler.ServeHTTP(w, r)
	})

	hErr := make(chan error)
	qErr := make(chan error)
	go func() {
		hErr <- httpServer.Serve(tlsConn)
	}()
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
