package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
