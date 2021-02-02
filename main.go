package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/lucas-clemente/quic-go/http3"
	flag "github.com/spf13/pflag"
)

const (
	uploadFormFileName = "originalFile"
)

var (
	workMode      string
	fileServePath string
	listenAddr    string
	serverAddr    string
	protocol      string
)

func printExamples() {
	fmt.Println("Examples:")
	fmt.Println("\ttransfer")
	fmt.Println("\ttransfer -m server -l :8888")
	fmt.Println("\ttransfer -m client -c http://172.16.0.1:8080/uploadFile ~/file-to-upload")
	fmt.Println("\ttransfer -m proxy")
	fmt.Println("\ttransfer -m relay 8080:172.16.0.1:8080 8081:172.16.0.2:8080 8082:172.16.0.3:8080")
}

func quicHandler() {
	switch workMode {
	case "client":
		args := flag.Args()
		if len(args) == 0 {
			log.Fatal("Local file to be uploaded is missing.")
		}
	case "server":
		log.Println("Starting quic(http3) server at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		http.HandleFunc("/uploadFile", uploadFileHandler)
		http.Handle("/", http.FileServer(http.Dir(fileServePath)))
		log.Fatal(http3.ListenAndServeQUIC(listenAddr, "cert/chain.pem", "cert/privkey.pem", nil))
	case "proxy":
		log.Println("Starting http proxy at", listenAddr, ", please don't close it if you are not sure what it is doing.")
	case "relay":
		args := flag.Args()
		if len(args) == 0 {
			log.Fatal("Port mapping is missing.")
		}
		log.Println("Starting http reverse proxy at", strings.Join(args, " "), ", please don't close it if you are not sure what it is doing.")
	}
}

func httpHandler() {
	switch workMode {
	case "client":
		args := flag.Args()
		if len(args) == 0 {
			log.Fatal("Local file to be uploaded is missing.")
		}
		for _, f := range args {
			uploadFileRequest(serverAddr, f)
		}
	case "server":
		log.Println("Starting http server at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		http.HandleFunc("/uploadFile", uploadFileHandler)
		http.Handle("/", http.FileServer(http.Dir(fileServePath)))
		log.Fatal(http.ListenAndServe(listenAddr, nil))
	case "proxy":
		log.Println("Starting http proxy at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		log.Fatal(http.ListenAndServe(listenAddr, createProxy()))
	case "relay":
		args := flag.Args()
		if len(args) == 0 {
			log.Fatal("Port mapping is missing.")
		}
		log.Println("Starting http reverse proxy at", strings.Join(args, " "), ", please don't close it if you are not sure what it is doing.")
		var wg sync.WaitGroup
		wg.Add(len(args))
		for _, a := range args {
			ss := strings.Split(a, ":")
			if len(ss) != 3 {
				log.Println("Drop invalid port mapping entry", a)
				wg.Done()
				continue
			}
			go createReverseProxy(fmt.Sprintf(":%s", ss[0]), fmt.Sprintf("http://%s:%s", ss[1], ss[2]), &wg)
		}
		wg.Wait()
	default:
		log.Fatal("Unsupported work mode, available values: server, client, proxy")
	}
}

func main() {
	help := false
	flag.StringVarP(&protocol, "protocol", "p", "http", "transfer protocol, candidates: http(/http1/http1.1/http2), kcp, quic(/http3)")
	flag.StringVarP(&workMode, "mode", "m", "server", "work mode, candidates: server, client, proxy, relay")
	flag.StringVarP(&fileServePath, "directory", "d", ".", "serve directory path, server/client mode only")
	flag.StringVarP(&listenAddr, "listen", "l", ":8080", "listen address, server/proxy mode only")
	flag.StringVarP(&serverAddr, "connect", "c", "", "upload server address, for example: http://172.16.0.1:8080/uploadFile, client mode only")
	flag.BoolVarP(&help, "help", "h", false, "show this help message")
	flag.Parse()

	if help {
		printExamples()
		fmt.Printf("\n")
		flag.PrintDefaults()
		return
	}

	switch strings.ToLower(protocol) {
	case "http", "http1", "http1.1", "http2":
		httpHandler()
	case "kcp":
	case "quic", "http3":
	default:
		log.Fatal("Unsupported protocol")
	}
}
