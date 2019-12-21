package main

import (
	"bytes"
	"os"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/lucas-clemente/quic-go"
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
	body, _ := rsp.Body.(*http3.Body)

	vbuf := &bytes.Buffer{}
	n, lossRange, err := Copy(vbuf, body, rsp)

	// lossRange, err := body.MyRead(buffer, rsp)
	fmt.Println("recv Bytes: ", n)
	fmt.Println("lossRange: ", lossRange)
	validBytes := calcValidBytes(n, lossRange)
	fmt.Println("validBytes: ", validBytes)
	fmt.Println("loss ratio: ", float64(n - validBytes) / float64(n))
	// _, err = io.Copy(body, rsp.Body) // ここでrsp.Body.Read()が呼ばれて、初めてバイトストリームからの読み出し
	if err != nil && err != io.EOF {
		panic(err)
	}
	f, err := os.OpenFile("movie.svc", os.O_WRONLY|os.O_CREATE, 0755)
	if err != nil {
		panic(err)
	}
	_, err = f.Write(vbuf.Bytes())
	if err != nil {
		panic(err)
	}

}

func calcValidBytes(n int64, byteRange []quic.ByteRange) int64 {
	for _, br := range byteRange {
		n -= int64(br.End - br.Start)
	}
	return n
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

func Copy(dst io.Writer, src *http3.Body, rsp *http.Response) (int64, []quic.ByteRange, error) {
	return copyBuffer(dst, src, nil, rsp)
}

// ここで帰るresultは最後の読み取りでのresult。lossRangeは呼ばれるたびに保存されてappendされてる
func copyBuffer(dst io.Writer, src io.Reader, buf []byte, rsp *http.Response) (int64, []quic.ByteRange, error) {
	if buf == nil {
		size := 32 * 1024
		if l, ok := src.(*io.LimitedReader); ok && int64(size) > l.N {
			if l.N < 1 {
				size = 1
			} else {
				size = int(l.N)
			}
		}
		buf = make([]byte, size)
	}
	lossRange := make([]quic.ByteRange, 0)
	var written int64
	var err error
	for {
		if body, ok := src.(*http3.Body); ok {
			result, er := body.MyRead(buf, rsp)
			lossRange = append(lossRange, result.LossRange...)
			nr := result.N
			if nr > 0 {
				nw, ew := dst.Write(buf[0:nr])
				if nw > 0 {
					written += int64(nw)
				}
				if ew != nil {
					err = ew
					break
				}
				if nr != nw {
					err = io.ErrShortWrite
					break
				}
			}
			if er != nil {
				if er != io.EOF {
					err = er
				}
				break
			}
		}
	}
	return written, lossRange, err
}
