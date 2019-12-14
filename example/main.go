package main

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
)

func h3serve(addr string) {
	elf, _ := os.Executable()
	currentDir := filepath.Dir(elf)
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(currentDir)))

	quicConf := &quic.Config{KeepAlive: true}
	fmt.Printf("listening on: %v\n", addr)
	server := http3.Server{
		Server:     &http.Server{Handler: mux, Addr: addr},
		QuicConfig: quicConf,
	}
	certFile := path.Join(currentDir, "cert.pem")
	keyFile := path.Join(currentDir, "key.pem")
	err := server.ListenAndServeTLS(certFile, keyFile)
	if err != nil {
		fmt.Println(err)
	}
}

func main() {
	fmt.Println("start")
	h3serve("0.0.0.0:6666")
}
