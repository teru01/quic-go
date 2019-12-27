package http3

import (
	"bytes"
	"time"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/marten-seemann/qpack"
)

type responseWriter struct {
	stream quic.Stream

	header        http.Header
	status        int // status code passed to WriteHeader
	headerWritten bool

	logger utils.Logger
}

var _ http.ResponseWriter = &responseWriter{}

func newResponseWriter(stream quic.Stream, logger utils.Logger) *responseWriter {
	return &responseWriter{
		header: http.Header{},
		stream: stream,
		logger: logger,
	}
}

func (w *responseWriter) Header() http.Header {
	return w.header
}

func (w *responseWriter) WriteHeader(status int) {
	if w.headerWritten {
		return
	}
	w.headerWritten = true
	w.status = status

	var headers bytes.Buffer
	enc := qpack.NewEncoder(&headers)
	enc.WriteField(qpack.HeaderField{Name: ":status", Value: strconv.Itoa(status)})

	for k, v := range w.header {
		for index := range v {
			enc.WriteField(qpack.HeaderField{Name: strings.ToLower(k), Value: v[index]})
		}
	}

	buf := &bytes.Buffer{}
	(&headersFrame{Length: uint64(headers.Len())}).Write(buf)
	w.logger.Infof("Responding with %d", status)
	if _, err := w.stream.Write(buf.Bytes()); err != nil {
		w.logger.Errorf("could not write headers frame: %s", err.Error())
	}
	if _, err := w.stream.Write(headers.Bytes()); err != nil {
		w.logger.Errorf("could not write header frame payload: %s", err.Error())
	}
}

var dur time.Duration

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(200)
	}
	if !bodyAllowedForStatus(w.status) {
		return 0, http.ErrBodyNotAllowed
	}
	df := &dataFrame{Length: uint64(len(p))}
	buf := &bytes.Buffer{}
	df.Write(buf)
	// VIDEO: ヘッダを解釈して選択的に送る
	if w.Header().Get("Transport-Response-Reliability") == "" {
		// VIDEO: Reliable write
		if _, err := w.stream.Write(buf.Bytes()); err != nil {
			return 0, err
		}
		s1 := time.Now()
		res, e := w.stream.Write(p)
		dur += time.Since(s1)
		fmt.Println("write time ", dur)
		return res, e

	} else {
		// VIDEO: データフレームヘッダはreliable write
		if _, err := w.stream.Write(buf.Bytes()); err != nil {
			return 0, err
		}
		// VIDEO: データフレームbodyはunreliable write
		s1 := time.Now()
		res, e := w.stream.UnreliableWrite(p)
		dur += time.Since(s1)
		fmt.Println("unrel write time ", dur)
		return res, e
	}
}

func (w *responseWriter) Flush() {}

// test that we implement http.Flusher
var _ http.Flusher = &responseWriter{}

// copied from http2/http2.go
// bodyAllowedForStatus reports whether a given response status code
// permits a body. See RFC 2616, section 4.4.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}
	return true
}
