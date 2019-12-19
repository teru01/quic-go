package http3

import (
	"fmt"
	"io"
	"net/http"

	"github.com/lucas-clemente/quic-go"
)

// The body of a http.Request or http.Response.
type Body struct {
	str quic.Stream

	isRequest bool

	// only set for the http.Response
	// The channel is closed when the user is done with this response:
	// either when Read() errors, or when Close() is called.
	reqDone       chan<- struct{}
	reqDoneClosed bool

	onFrameError func()

	bytesRemainingInFrame uint64
}

var _ io.ReadCloser = &Body{}

func newRequestBody(str quic.Stream, onFrameError func()) *Body {
	return &Body{
		str:          str,
		onFrameError: onFrameError,
		isRequest:    true,
	}
}

func newResponseBody(str quic.Stream, done chan<- struct{}, onFrameError func()) *Body {
	return &Body{
		str:          str,
		onFrameError: onFrameError,
		reqDone:      done,
	}
}

func (r *Body) myReadImpl(b []byte, resp *http.Response) (*quic.UnreliableReadResult, error) {
	if r.bytesRemainingInFrame == 0 {
	parseLoop:
		for {
			frame, err := parseNextFrame(r.str)
			if err != nil {
				return nil, err
			}
			switch f := frame.(type) {
			case *headersFrame:
				// skip HEADERS frames
				continue
			case *dataFrame:
				r.bytesRemainingInFrame = f.Length
				break parseLoop
			default:
				r.onFrameError()
				// parseNextFrame skips over unknown frame types
				// Therefore, this condition is only entered when we parsed another known frame type.
				return nil, fmt.Errorf("peer sent an unexpected frame: %T", f)
			}
		}
	}
	var n int
	var err error
	if resp.Header.Get("Transport-Response-Reliability") == "" {
		// VIDEO: Reliable read
		if r.bytesRemainingInFrame < uint64(len(b)) {
			n, err = r.str.Read(b[:r.bytesRemainingInFrame])
		} else {
			n, err = r.str.Read(b)
		}
		r.bytesRemainingInFrame -= uint64(n)
		return &quic.UnreliableReadResult{N: n, LossRange: nil}, err
	} else {
		// VIDEO: unreliable read
		var readResult quic.UnreliableReadResult
		if r.bytesRemainingInFrame < uint64(len(b)) {
			readResult, err = r.str.UnreliableRead(b[:r.bytesRemainingInFrame])
		} else {
			readResult, err = r.str.UnreliableRead(b)
		}
		r.bytesRemainingInFrame -= uint64(n)
		return &readResult, err
	}
}

func (r *Body) MyRead(b []byte, resp *http.Response) (*quic.UnreliableReadResult, error) {
	n, err := r.myReadImpl(b, resp)
	if err != nil && !r.isRequest {
		r.requestDone()
	}
	return n, err
}

func (r *Body) Read(b []byte) (int, error) {
	n, err := r.readImpl(b)
	if err != nil && !r.isRequest {
		r.requestDone()
	}
	return n, err
}

// VIDEO: この中でrecv_streamのReadが呼ばれる
// 全て読みきるわけではない
func (r *Body) readImpl(b []byte) (int, error) {
	if r.bytesRemainingInFrame == 0 {
	parseLoop:
		for {
			frame, err := parseNextFrame(r.str)
			if err != nil {
				return 0, err
			}
			switch f := frame.(type) {
			case *headersFrame:
				// skip HEADERS frames
				continue
			case *dataFrame:
				r.bytesRemainingInFrame = f.Length
				break parseLoop
			default:
				r.onFrameError()
				// parseNextFrame skips over unknown frame types
				// Therefore, this condition is only entered when we parsed another known frame type.
				return 0, fmt.Errorf("peer sent an unexpected frame: %T", f)
			}
		}
	}

	var n int
	var err error
	if r.bytesRemainingInFrame < uint64(len(b)) {
		n, err = r.str.Read(b[:r.bytesRemainingInFrame])
	} else {
		n, err = r.str.Read(b)
	}
	r.bytesRemainingInFrame -= uint64(n)
	return n, err
}

func (r *Body) requestDone() {
	if r.reqDoneClosed {
		return
	}
	close(r.reqDone)
	r.reqDoneClosed = true
}

func (r *Body) Close() error {
	// quic.Stream.Close() closes the write side, not the read side
	if r.isRequest {
		return r.str.Close()
	}
	r.requestDone()
	r.str.CancelRead(quic.ErrorCode(errorRequestCanceled))
	return nil
}
