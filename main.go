package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	flag "github.com/spf13/pflag"
)

var (
	workMode      string
	fileServePath string
	listenAddr    string
	serverAddr    string
)

func uploadFileHandler(w http.ResponseWriter, r *http.Request) {

	//upload size
	err := r.ParseMultipartForm(200000) // grab the multipart form
	if err != nil {
		fmt.Fprintln(w, err)
	}

	//reading original file
	file, handler, err := r.FormFile("originalFile")
	if err != nil {
		fmt.Println("Error Retrieving the File")
		fmt.Println(err)
		return
	}
	defer file.Close()

	resFile, err := os.Create(filepath.Join(fileServePath, handler.Filename))
	if err != nil {
		fmt.Fprintln(w, err)
	}
	defer resFile.Close()
	if err == nil {
		io.Copy(resFile, file)
		defer resFile.Close()
		fmt.Fprintf(w, "Successfully Uploaded Original File\n")
	}
}

// Creates a new file upload http request with optional extra params
func newfileUploadRequest(uri string, params map[string]string, paramName, filePath string) (*http.Request, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(paramName, filepath.Base(filePath))
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(part, file)

	for key, val := range params {
		_ = writer.WriteField(key, val)
	}
	err = writer.Close()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", uri, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, err
}

func uploadFileRequest(uri string, filePath string) {
	extraParams := map[string]string{
		"title":       filepath.Base(filePath),
		"author":      "CUBE SA",
		"description": fmt.Sprintf("file %s uploaded by CUBE SA", filepath.Base(filePath)),
	}
	request, err := newfileUploadRequest(uri, extraParams, "originalFile", filePath)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{}
	resp, err := client.Do(request)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	log.Println(string(body))
}

func printExamples() {
	fmt.Println("Examples:")
	fmt.Println("\ttransfer")
	fmt.Println("\ttransfer -m server -l :8888")
	fmt.Println("\ttransfer -m client -c http://172.16.0.1:8080/uploadFile ~/file-to-upload")
	fmt.Println("\ttransfer -m proxy")
	fmt.Println("\ttransfer -m relay 8080:172.16.0.1:8080 8081:172.16.0.2:8080 8082:172.16.0.3:8080")
}

func main() {
	help := false
	flag.StringVarP(&workMode, "mode", "m", "server", "work mode, candidates: server, client, proxy, relay")
	flag.StringVarP(&fileServePath, "path", "p", ".", "file serve path, server/client mode only")
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

	switch workMode {
	case "client":
		args := flag.Args()
		for _, f := range args {
			uploadFileRequest(serverAddr, f)
		}
	case "server":
		log.Println("Starting http server at", listenAddr, ", please don't close it if you are not sure what it does.")

		http.HandleFunc("/uploadFile", uploadFileHandler)
		fs := http.FileServer(http.Dir(fileServePath))
		http.Handle("/", fs)
		log.Fatal(http.ListenAndServe(listenAddr, nil))
	case "proxy":
		log.Println("Starting http proxy at", listenAddr, ", please don't close it if you are not sure what it does.")
	case "relay":
		args := flag.Args()
		log.Println("Starting relay server at", strings.Join(args, " "), ", please don't close it if you are not sure what it does.")
	default:

		log.Fatal("Unsupported work mode, available values: server, client, proxy")
	}
}
