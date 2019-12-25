package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	
	"sync"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/lucas-clemente/quic-go/internal/utils"
)

func main() {
	verbose := flag.Bool("v", false, "verbose")
	unreliable := flag.Bool("u", false, "unreliable")
	loop := flag.Bool("l", false, "loop")
	r := flag.Int("r", 1, "times")
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

	for p := 0; ; p++ {
		wg := &sync.WaitGroup{}
		// fmt.Fprintln(os.Stderr, p)
		for q := 0; q < *r; q++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fmt.Printf("##################### %v ##################3#\n", q)
				// reqにContextで渡す
				rsp, err := http3.GetWithReliability(hclient, url, *unreliable)

				if err != nil {
					panic(err)
				}
				// logger.Infof("Got response for %s: %#v", url, rsp)
				body, _ := rsp.Body.(*http3.Body)

				vbuf := &bytes.Buffer{}
				n, lossRange, err := http3.Copy(vbuf, body, rsp)

				// lossRange, err := body.MyRead(buffer, rsp)
				fmt.Println("recv Bytes: ", n)
				fmt.Println("lossRange: ", lossRange)
				validBytes := calcValidBytes(n, lossRange)
				fmt.Println("validBytes: ", validBytes)
				fmt.Println("loss ratio: ", float64(n-validBytes)/float64(n))
				// _, err = io.Copy(body, rsp.Body) // ここでrsp.Body.Read()が呼ばれて、初めてバイトストリームからの読み出し
				if err != nil && err != io.EOF {
					panic(err)
				}
				// err = ioutil.WriteFile("movie.svc", vbuf.Bytes(), 0644)
				if err != nil {
					panic(err)
				}
			}()
		}
		wg.Wait()

		if !*loop {
			break
		}
	}
}

func calcValidBytes(n int64, byteRange []quic.ByteRange) int64 {
	for _, br := range byteRange {
		n -= int64(br.End - br.Start)
	}
	return n
}
