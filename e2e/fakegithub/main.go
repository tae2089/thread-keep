package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18081", "listen address")
	flag.Parse()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/acme/thread-keep" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Header.Get("Authorization") {
		case "Bearer writer-token":
			_, _ = fmt.Fprint(writer, `{"permissions":{"push":true,"pull":true}}`)
		case "Bearer reader-token":
			_, _ = fmt.Fprint(writer, `{"permissions":{"push":false,"pull":true}}`)
		default:
			writer.WriteHeader(http.StatusUnauthorized)
		}
	})
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "fake github listening on %s\n", listener.Addr())
	if err := http.Serve(listener, handler); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
