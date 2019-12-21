package http3

import (
	"io"
	"net/http"
	"context"

	"github.com/lucas-clemente/quic-go"
)

func Copy(dst io.Writer, src *Body, rsp *http.Response) (int64, []quic.ByteRange, error) {
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
		if body, ok := src.(*Body); ok {
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

func GetWithReliability(hclient *http.Client, url string, unreliable bool) (*http.Response, error) {
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
