package main

import (
	"context"
	"io"
	"crypto/tls"
	"flag"
	"net/http"
	"fmt"

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
		url = "https://localhost:6666/hoge.html"
	}

	// reqにContextで渡す
	rsp, err := get(hclient, url, *unreliable)

	if err != nil {
		panic(err)
	}
	// logger.Infof("Got response for %s: %#v", url, rsp)
	oneMB := int64(1.049e+6) // VIDEO: セグメントは1MB以内と仮定
	buffer := make([]byte, oneMB)
	body, _ := rsp.Body.(*http3.Body)

	result, err := body.MyRead(buffer, rsp)
	fmt.Println(result.LossRange)
	// _, err = io.Copy(body, rsp.Body) // ここでrsp.Body.Read()が呼ばれて、初めてバイトストリームからの読み出し
	if err != nil && err != io.EOF{
		panic(err)
	}
	logger.Infof("%s", buffer)
}

func get(hclient *http.Client, url string, unreliable bool) (*http.Response, error) {
	ctx := context.WithValue(context.Background(), "unreliable_key", unreliable)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if unreliable {
		req.Header.Add("Transport-Response-Reliability", "unreliable")
	}
	if err != nil {
		return nil, err
	}
	return hclient.Do(req)
}

// func UnreliableGet(hclient *http.Client, url string) {
// 	resp := hclient.UnreliableGet()
// 	resp.Body.My
// 	lossRange := resp.lossRange
// 	body := &bytes.Buffer{}
// 	// resp.Body.Read(body)
// 	// io.Copy(body, resp.Body)
// }
