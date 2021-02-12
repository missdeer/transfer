package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	flag "github.com/spf13/pflag"
)

const (
	uploadFormFileName = "originalFile"
)

var (
	workMode           string
	fileServePath      string
	listenAddr         string
	serverAddr         string
	protocol           string
	certFile           string
	keyFile            string
	outputFile         string
	insecureSkipVerify bool
)

func printExamples() {
	fmt.Println("Examples:")
	fmt.Println("\ttransfer")
	fmt.Println("\ttransfer -m server -l :8888")
	fmt.Println("\ttransfer -m upload -c http://172.16.0.1:8080/uploadFile ~/file-to-upload")
	fmt.Println("\ttransfer -m download -c http://172.16.0.1:8080/file-to-download -o ~/file-downloaded")
	fmt.Println("\ttransfer -m proxy")
	fmt.Println("\ttransfer -m relay 8080:172.16.0.1:8080 8081:172.16.0.2:8080 8082:172.16.0.3:8080")
}

func httpsHandler(quicOnly bool) {
	switch workMode {
	case "server":
		log.Println("Starting quic(http3) server at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		http.HandleFunc("/uploadFile", uploadFileHandler)
		http.Handle("/", http.FileServer(http.Dir(fileServePath)))
		log.Fatal(listenAndServe(listenAddr, certFile, keyFile, nil, quicOnly))
	case "proxy":
		log.Println("Starting http proxy at", listenAddr, ", please don't close it if you are not sure what it is doing.")
		log.Fatal(listenAndServe(listenAddr, certFile, keyFile, createProxy(), quicOnly))
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
			go createReverseProxy(fmt.Sprintf(":%s", ss[0]), fmt.Sprintf("https://%s:%s", ss[1], ss[2]), &wg, true, quicOnly)
		}
		wg.Wait()
	}
}

func httpHandler() {
	switch workMode {
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
			go createReverseProxy(fmt.Sprintf(":%s", ss[0]), fmt.Sprintf("http://%s:%s", ss[1], ss[2]), &wg, false, false)
		}
		wg.Wait()
	default:
		log.Fatal("Unsupported work mode, available values: server, client, proxy")
	}
}

func main() {
	help := false
	flag.StringVarP(&protocol, "protocol", "p", "http", "transfer protocol, candidates: http, https, quic")
	flag.StringVarP(&workMode, "mode", "m", "server", "work mode, candidates: server, download, upload, proxy, relay")
	flag.StringVarP(&fileServePath, "directory", "d", ".", "serve directory path, server mode only")
	flag.StringVarP(&listenAddr, "listen", "l", ":8080", "listen address, server/proxy mode only")
	flag.StringVarP(&serverAddr, "connect", "c", "", "upload server address, for example: http://172.16.0.1:8080/uploadFile, download/upload mode only")
	flag.StringVarP(&outputFile, "output", "o", "", "save downloaded file to local path, download mode only")
	flag.StringVarP(&certFile, "cert", "t", "cert.pem", "SSL certificate file path")
	flag.StringVarP(&keyFile, "key", "k", "key.pem", "SSL key file path")
	flag.BoolVarP(&insecureSkipVerify, "insecureSkipVerify", "", false, "insecure skip SSL verify")
	flag.BoolVarP(&help, "help", "h", false, "show this help message")
	flag.Parse()

	if help {
		printExamples()
		fmt.Printf("\n")
		flag.PrintDefaults()
		return
	}
	switch workMode {
	case "download":
		uri, isHTTP3, _ := isHTTP3Enabled(serverAddr)
		log.Printf("downloading %s to %s, isHTTP3Enabled=%t\n", uri, outputFile, isHTTP3)
		downloadFileRequest(uri, outputFile, isHTTP3)
		return
	case "upload":
		uri, isHTTP3, _ := isHTTP3Enabled(serverAddr)
		args := flag.Args()
		for _, f := range args {
			log.Printf("uploading %s to %s, isHTTP3Enabled=%t\n", f, uri, isHTTP3)
			uploadFileRequest(uri, f, isHTTP3)
		}
		return
	default:
	}

	switch strings.ToLower(protocol) {
	case "http":
		httpHandler()
	case "https":
		httpsHandler(false)
	case "quic":
		httpsHandler(true)
	default:
		log.Fatal("Unsupported protocol")
	}
}
