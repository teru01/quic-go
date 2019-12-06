package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"io"
	"net/http"

	"github.com/lucas-clemente/quic-go/http3"
	"github.com/lucas-clemente/quic-go/internal/testdata"
	"github.com/lucas-clemente/quic-go/internal/utils"
)

func main() {
	verbose := flag.Bool("v", false, "verbose")
	quiet := flag.Bool("q", false, "don't print the data")
	insecure := flag.Bool("insecure", false, "skip certificate verification")
	flag.Parse()
	urls := flag.Args()

	logger := utils.DefaultLogger

	if *verbose {
		logger.SetLogLevel(utils.LogLevelDebug)
	} else {
		logger.SetLogLevel(utils.LogLevelInfo)
	}
	logger.SetLogTimeFormat("")

	pool, err := x509.SystemCertPool()
	if err != nil {
		panic(err)
	}
	testdata.AddRootCA(pool)
	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: *insecure,
		},
	}
	defer roundTripper.Close()
	hclient := &http.Client{
		Transport: roundTripper,
	}
	_ = urls
	url := "https://localhost:6121"

	// reqにContextで渡す
	ctx := context.WithValue(context.Background(), "unreliable_key", true)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)

	rsp, err := hclient.Do(req)

	if err != nil {
		panic(err)
	}
	logger.Infof("Got response for %s: %#v", url, rsp)

	body := &bytes.Buffer{}
	_, err = io.Copy(body, rsp.Body)
	if err != nil {
		panic(err)
	}
	if *quiet {
		logger.Infof("Request Body: %d bytes", body.Len())
	} else {
		logger.Infof("Request Body:")
		logger.Infof("%s", body.Bytes())
	}
}
