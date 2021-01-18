package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func uploadFileHandler(w http.ResponseWriter, r *http.Request) {

	//upload size
	err := r.ParseMultipartForm(200000) // grab the multipart form
	if err != nil {
		fmt.Fprintln(w, err)
	}

	//reading original file
	file, handler, err := r.FormFile(uploadFormFileName)
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
