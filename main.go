package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	flag "github.com/spf13/pflag"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
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
	referrer           string
	cookie             string
	userAgent          string
	insecureSkipVerify bool
	reuseThread        bool
	autoHTTP3          bool
	concurrentThread   int
	retryTimes         int
	readBufSize        int64
	leastTryBufferSize int64
	continueAt         int64
	headers            []string

	englishPrinter = message.NewPrinter(language.English)
	logStderr      = log.New(os.Stderr, "", 0)
	logStdout      = log.New(os.Stdout, "", 0)
)

func printExamples() {
	fmt.Println("Examples:")
	fmt.Println("\ttransfer")
	fmt.Println("\ttransfer -m server -l :8888")
	fmt.Println("\ttransfer -m upload -c http://172.16.0.1:8080/uploadFile ~/file-to-upload")
	fmt.Println("\ttransfer -m download -c http://172.16.0.1:8080/file-to-download -o ~/file-downloaded")
	fmt.Println("\ttransfer -m proxy")
	fmt.Println("\ttransfer -m relay 8080<->http://172.16.0.1:8080 8081<->http://172.16.0.2:8080 8082<->http://172.16.0.3:8080")
}

func ternaryOp(condition bool, v1, v2 string) string {
	if condition {
		return v1
	}
	return v2
}

func httpsHandler(quicOnly bool) {
	switch workMode {
	case "server":
		logStdout.Println("Starting ", ternaryOp(quicOnly, "quic", "https"), " server at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		http.HandleFunc("/uploadFile", uploadFileHandler)
		http.Handle("/", http.FileServer(http.Dir(fileServePath)))
		logStderr.Fatal(listenAndServe(listenAddr, certFile, keyFile, nil, quicOnly))
	case "proxy":
		logStdout.Println("Starting http proxy at", listenAddr, ", please don't close it if you are not sure what it is doing.")
		logStderr.Fatal(listenAndServe(listenAddr, certFile, keyFile, createProxy(), quicOnly))
	case "relay":
		args := flag.Args()
		if len(args) == 0 {
			logStderr.Fatal("Port mapping is missing.")
		}
		logStdout.Println("Starting http reverse proxy at", strings.Join(args, " "), ", please don't close it if you are not sure what it is doing.")
		var wg sync.WaitGroup
		wg.Add(len(args))
		for _, a := range args {
			ss := strings.Split(a, "<->")
			if len(ss) != 2 {
				logStdout.Println("Drop invalid port mapping entry", a)
				wg.Done()
				continue
			}
			go createReverseProxy(func(mux *http.ServeMux) error {
				return listenAndServe(fmt.Sprintf(":%s", ss[0]), certFile, keyFile, mux, quicOnly)
			}, ss[1], &wg)
		}
		wg.Wait()
	}
}

func httpHandler() {
	switch workMode {
	case "server":
		logStdout.Println("Starting http server at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		http.HandleFunc("/uploadFile", uploadFileHandler)
		http.Handle("/", http.FileServer(http.Dir(fileServePath)))
		logStderr.Fatal(http.ListenAndServe(listenAddr, nil))
	case "proxy":
		logStdout.Println("Starting http proxy at", listenAddr, ", please don't close it if you are not sure what it is doing.")

		logStderr.Fatal(http.ListenAndServe(listenAddr, createProxy()))
	case "relay":
		args := flag.Args()
		if len(args) == 0 {
			logStderr.Fatal("Port mapping is missing.")
		}
		logStdout.Println("Starting http reverse proxy at", strings.Join(args, " "), ", please don't close it if you are not sure what it is doing.")
		var wg sync.WaitGroup
		wg.Add(len(args))
		for _, a := range args {
			ss := strings.Split(a, "<->")
			if len(ss) != 2 {
				logStdout.Println("Drop invalid port mapping entry", a)
				wg.Done()
				continue
			}
			go createReverseProxy(func(mux *http.ServeMux) error {
				s := http.Server{
					Addr:    fmt.Sprintf(":%s", ss[0]),
					Handler: mux,
				}
				return s.ListenAndServe()
			}, ss[1], &wg)
		}
		wg.Wait()
	default:
		logStderr.Fatal("Unsupported work mode, available values: server, client, proxy")
	}
}

func main() {
	help := false
	flag.StringArrayVarP(&headers, "header", "H", []string{}, "Add header to request")
	flag.StringVarP(&protocol, "protocol", "p", "http", "transfer protocol, candidates: http, https, quic")
	flag.StringVarP(&workMode, "mode", "m", "download", "work mode, candidates: server, download, upload, proxy, relay")
	flag.StringVarP(&fileServePath, "directory", "d", ".", "serve directory path, server mode only")
	flag.StringVarP(&listenAddr, "listen", "l", ":8080", "listen address, server/proxy mode only")
	flag.StringVarP(&serverAddr, "connect", "c", "", "upload server address, for example: http://172.16.0.1:8080/uploadFile, download/upload mode only")
	flag.StringVarP(&outputFile, "output", "o", "", "save downloaded file to local path, leave blank to extract file name from URL path, download mode only")
	flag.StringVarP(&certFile, "cert", "t", "cert.pem", "SSL certificate file path")
	flag.StringVarP(&keyFile, "key", "k", "key.pem", "SSL key file path")
	flag.IntVarP(&concurrentThread, "thread", "x", 1, "download concurrent thread count, download mode only")
	flag.IntVarP(&retryTimes, "retry", "r", math.MaxInt, "retry times, if < 0, means infinitely")
	flag.Int64VarP(&readBufSize, "readBufSize", "B", 8*1024, "read buffer size ~ [4096, 32768], download mode only")
	flag.BoolVarP(&insecureSkipVerify, "insecureSkipVerify", "", false, "insecure skip SSL verify")
	flag.BoolVarP(&reuseThread, "reuseThread", "", true, "reuse thread, download mode only")
	flag.BoolVarP(&autoHTTP3, "autoHTTP3", "", true, "auto enable HTTP3, download mode only")
	flag.StringVarP(&referrer, "referrer", "e", "", "Referrer URL, download mode only")
	flag.StringVarP(&cookie, "cookie", "b", "", "Send cookies from string/file, download mode only")
	flag.StringVarP(&userAgent, "userAgent", "A", "", "Send User-Agent <name> to server, download mode only")
	flag.Int64VarP(&continueAt, "continueAt", "C", 0, "Resume downloading from byte position <N>, download mode only")
	flag.BoolVarP(&help, "help", "h", false, "show this help message")
	flag.Parse()

	if help {
		printExamples()
		fmt.Printf("\n")
		flag.PrintDefaults()
		return
	}
	if serverAddr == "" && (workMode == "download" || workMode == "upload") && flag.NArg() == 1 {
		serverAddr = flag.Arg(0)
	}
	switch workMode {
	case "download":
		uri := serverAddr
		if uri == "" {
			if len(flag.Args()) == 1 {
				uri = flag.Args()[0]
			} else {
				logStderr.Fatal("Download server address is missing.")
			}
		}
		isHTTP3 := false
		if readBufSize > 32*1024 {
			readBufSize = 32 * 1024
		}
		if readBufSize < 4*1024 {
			readBufSize = 4 * 1024
		}
		leastTryBufferSize = readBufSize * 10

		if outputFile == "" {
			outputFile = filepath.Base(uri)
		}

		var contentLength int64 = 0
		respHeaders, err := getHTTPResponseHeader(uri)
		if err == nil {
			for k, v := range respHeaders {
				fmt.Println("response header:", k, v)
			}
			if strings.ToLower(protocol) == "quic" {
				isHTTP3 = true
			} else if autoHTTP3 {
				uri, isHTTP3, _ = isHTTP3Enabled(uri, respHeaders)
			}
			contentLength, _ = getContentLength(respHeaders)
		}

		if needDownload(respHeaders, contentLength, outputFile) {
			logs := englishPrinter.Sprintf("downloading %s to %s, isHTTP3Enabled=%t\n", uri, outputFile, isHTTP3)
			logStdout.Println(logs)
			downloadFileRequest(uri, contentLength, outputFile, isHTTP3)
		}
		return
	case "upload":
		uri := serverAddr
		isHTTP3 := false
		if strings.ToLower(protocol) == "quic" {
			isHTTP3 = true
		} else {
			if headers, err := getHTTPResponseHeader(serverAddr); err == nil {
				uri, isHTTP3, _ = isHTTP3Enabled(serverAddr, headers)
			}
		}
		args := flag.Args()
		for _, f := range args {
			logStdout.Printf("uploading %s to %s, isHTTP3Enabled=%t\n", f, uri, isHTTP3)
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
		logStderr.Fatal("Unsupported protocol")
	}
}
