package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"io"
	"net/http"

	"github.com/lucas-clemente/quic-go/http3"
	"github.com/lucas-clemente/quic-go/internal/utils"
)

func main() {
	verbose := flag.Bool("v", false, "verbose")
	unreliable := flag.Bool("u", false, "unreliable")
	flag.Parse()
	urls := flag.Args()

	logger := utils.DefaultLogger

	if *verbose {
		logger.SetLogLevel(utils.LogLevelDebug)
	} else {
		logger.SetLogLevel(utils.LogLevelInfo)
	}
	logger.SetLogTimeFormat("")

	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	defer roundTripper.Close()
	hclient := &http.Client{
		Transport: roundTripper,
	}
	_ = urls
	var url string
	if len(urls) == 1 {
		url = urls[0]
	} else {
		url = "https://localhost:6121"
	}

	// reqにContextで渡す
	rsp, err := get(hclient, url, *unreliable)

	if err != nil {
		panic(err)
	}
	// logger.Infof("Got response for %s: %#v", url, rsp)

	body := &bytes.Buffer{}
	_, err = io.Copy(body, rsp.Body)
	if err != nil {
		panic(err)
	}
	logger.Infof("%s", body.Bytes())
}

func get(hclient *http.Client, url string, unreliable bool) (*http.Response, error){
	ctx := context.WithValue(context.Background(), "unreliable_key", unreliable)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	return hclient.Do(req)
}
