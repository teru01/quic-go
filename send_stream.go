// +build !debug

package quic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lucas-clemente/quic-go/internal/ackhandler"

	"github.com/lucas-clemente/quic-go/internal/flowcontrol"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
)

type sendStreamI interface {
	SendStream
	handleStopSendingFrame(*wire.StopSendingFrame)
	hasData() bool
	popStreamFrame(maxBytes protocol.ByteCount) (*ackhandler.Frame, bool)
	closeForShutdown(error)
	handleMaxStreamDataFrame(*wire.MaxStreamDataFrame)
}

type sendStream struct {
	mutex sync.Mutex

	numOutstandingFrames int64
	unreliableMustAckedNum int64
	retransmissionQueue  []wire.StreamFrameInterface

	ctx       context.Context
	ctxCancel context.CancelFunc

	streamID protocol.StreamID
	sender   streamSender

	writeOffset protocol.ByteCount

	cancelWriteErr      error
	closeForShutdownErr error

	closedForShutdown bool // set when CloseForShutdown() is called
	finishedWriting   bool // set once Close() is called
	canceledWrite     bool // set when CancelWrite() is called, or a STOP_SENDING frame is received
	finSent           bool // set when a STREAM_FRAME with FIN bit has been sent
	completed         bool // set when this stream has been reported to the streamSender as completed

	dataForWriting          []byte
	isCurrentDataUnreliable bool

	writeChan chan struct{}
	deadline  time.Time

	flowController flowcontrol.StreamFlowController

	version protocol.VersionNumber

	unreliable bool
}

var _ SendStream = &sendStream{}
var _ sendStreamI = &sendStream{}

func newSendStream(
	streamID protocol.StreamID,
	sender streamSender,
	flowController flowcontrol.StreamFlowController,
	version protocol.VersionNumber,
	unreliable bool,
) *sendStream {
	s := &sendStream{
		streamID:                streamID,
		sender:                  sender,
		flowController:          flowController,
		isCurrentDataUnreliable: false,
		writeChan:               make(chan struct{}, 1),
		version:                 version,
		unreliable:              unreliable,
	}
	s.ctx, s.ctxCancel = context.WithCancel(context.Background())
	return s
}

func (s *sendStream) StreamID() protocol.StreamID {
	return s.streamID // same for receiveStream and sendStream
}

func (s *sendStream) Write(p []byte) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.finishedWriting {
		return 0, fmt.Errorf("write on closed stream %d", s.streamID)
	}
	if s.canceledWrite {
		return 0, s.cancelWriteErr
	}
	if s.closeForShutdownErr != nil {
		return 0, s.closeForShutdownErr
	}
	if !s.deadline.IsZero() && !time.Now().Before(s.deadline) {
		return 0, errDeadline
	}
	if len(p) == 0 {
		return 0, nil
	}

	s.dataForWriting = p

	var (
		deadlineTimer  *utils.Timer
		bytesWritten   int
		notifiedSender bool
	)
	for {
		bytesWritten = len(p) - len(s.dataForWriting)
		deadline := s.deadline
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				s.dataForWriting = nil
				return bytesWritten, errDeadline
			}
			if deadlineTimer == nil {
				deadlineTimer = utils.NewTimer()
			}
			deadlineTimer.Reset(deadline)
		}
		if s.dataForWriting == nil || s.canceledWrite || s.closedForShutdown {
			break
		}

		var tmp sync.Mutex
		tmp.Lock()
		s.isCurrentDataUnreliable = false
		tmp.Unlock()

		s.mutex.Unlock()
		if !notifiedSender {
			s.sender.onHasStreamData(s.streamID) // must be called without holding the mutex
			notifiedSender = true
		}
		if deadline.IsZero() {
			<-s.writeChan
		} else {
			select {
			case <-s.writeChan:
			case <-deadlineTimer.Chan():
				deadlineTimer.SetRead()
			}
		}
		s.mutex.Lock()
	}

	if s.closeForShutdownErr != nil {
		return bytesWritten, s.closeForShutdownErr
	} else if s.cancelWriteErr != nil {
		return bytesWritten, s.cancelWriteErr
	}
	return bytesWritten, nil
}

func (s *sendStream) UnreliableWrite(p []byte) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.finishedWriting {
		return 0, fmt.Errorf("write on closed stream %d", s.streamID)
	}
	if s.canceledWrite {
		return 0, s.cancelWriteErr
	}
	if s.closeForShutdownErr != nil {
		return 0, s.closeForShutdownErr
	}
	if !s.deadline.IsZero() && !time.Now().Before(s.deadline) {
		return 0, errDeadline
	}
	if len(p) == 0 {
		return 0, nil
	}

	s.dataForWriting = p

	var (
		deadlineTimer  *utils.Timer
		bytesWritten   int
		notifiedSender bool
	)
	for {
		bytesWritten = len(p) - len(s.dataForWriting)
		deadline := s.deadline
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				s.dataForWriting = nil
				return bytesWritten, errDeadline
			}
			if deadlineTimer == nil {
				deadlineTimer = utils.NewTimer()
			}
			deadlineTimer.Reset(deadline)
		}
		if s.dataForWriting == nil || s.canceledWrite || s.closedForShutdown {
			break
		}

		// unreliable write
		var tmp sync.Mutex
		tmp.Lock()
		s.isCurrentDataUnreliable = true
		tmp.Unlock()

		s.mutex.Unlock()
		if !notifiedSender {
			s.sender.onHasStreamData(s.streamID) // must be called without holding the mutex
			notifiedSender = true
		}
		if deadline.IsZero() {
			<-s.writeChan
		} else {
			select {
			case <-s.writeChan:
			case <-deadlineTimer.Chan():
				deadlineTimer.SetRead()
			}
		}
		s.mutex.Lock()
	}

	if s.closeForShutdownErr != nil {
		return bytesWritten, s.closeForShutdownErr
	} else if s.cancelWriteErr != nil {
		return bytesWritten, s.cancelWriteErr
	}
	return bytesWritten, nil
}

// popStreamFrame returns the next STREAM frame that is supposed to be sent on this stream
// maxBytes is the maximum length this frame (including frame header) will have.
func (s *sendStream) popStreamFrame(maxBytes protocol.ByteCount) (*ackhandler.Frame, bool /* has more data to send */) {
	s.mutex.Lock()
	f, hasMoreData := s.popNewOrRetransmittedStreamFrame(maxBytes)
	if f != nil {
		s.numOutstandingFrames++ //ACKされてないフレーム数？
	}
	s.mutex.Unlock()

	if f == nil {
		return nil, hasMoreData
	}

	stFrame := f.(wire.StreamFrameInterface) // 必ず成功
	fmt.Printf("VIDEO: send_stream.go: currentDataUnreliable: %v len: %v\n", s.isCurrentDataUnreliable, stFrame.GetDataLen())

	// finビットの立っていないStreamFrame, フレームレベルでUnreliableである必要がある
	if s.unreliable && !stFrame.GetFinBit() && s.isCurrentDataUnreliable {
		// 再送を行わない
		return &ackhandler.Frame{Frame: f, OnLost: func(f wire.Frame) {}, OnAcked: s.frameAcked}, hasMoreData
	}
	if s.unreliable && stFrame.GetFinBit() {
		fmt.Println("VIDEO: send_stream.go: send fin reliably")
	}
	s.unreliableMustAckedNum++ // 必ずACKされないといけないフレーム数
	return &ackhandler.Frame{Frame: f, OnLost: s.queueRetransmission, OnAcked: s.frameAcked}, hasMoreData
}

func (s *sendStream) popNewOrRetransmittedStreamFrame(maxBytes protocol.ByteCount) (wire.StreamFrameInterface, bool /* has more data to send */) {
	var frameInterface wire.StreamFrameInterface

	if len(s.retransmissionQueue) > 0 {
		f, hasMoreRetransmissions := s.maybeGetRetransmission(maxBytes)
		if f != nil || hasMoreRetransmissions {
			if f == nil {
				return nil, true
			}
			// We always claim that we have more data to send.
			// This might be incorrect, in which case there'll be a spurious call to popStreamFrame in the future.
			frameInterface = f
			return frameInterface, true
		}
	}
	f := wire.GetStreamFrame()
	f.FinBit = false
	f.StreamID = s.streamID
	f.Offset = s.writeOffset
	f.DataLenPresent = true
	f.Data = f.Data[:0]

	hasMoreData := s.popNewStreamFrame(f, maxBytes)

	if len(f.Data) == 0 && !f.FinBit {
		f.PutBack()
		return nil, hasMoreData
	}
	if s.unreliable {
		frameInterface = &wire.UnreliableStreamFrame{f}
	} else {
		frameInterface = f
	}
	return frameInterface, hasMoreData
}

func (s *sendStream) popNewStreamFrame(f *wire.StreamFrame, maxBytes protocol.ByteCount) bool {
	if s.canceledWrite || s.closeForShutdownErr != nil {
		return false
	}

	maxDataLen := f.MaxDataLen(maxBytes, s.version)
	if maxDataLen == 0 { // a STREAM frame must have at least one byte of data
		return s.dataForWriting != nil
	}
	s.getDataForWriting(f, maxDataLen)
	if len(f.Data) == 0 && !f.FinBit {
		// this can happen if:
		// - popStreamFrame is called but there's no data for writing
		// - there's data for writing, but the stream is stream-level flow control blocked
		// - there's data for writing, but the stream is connection-level flow control blocked
		if s.dataForWriting == nil {
			return false
		}
		if isBlocked, offset := s.flowController.IsNewlyBlocked(); isBlocked {
			s.sender.queueControlFrame(&wire.StreamDataBlockedFrame{
				StreamID:  s.streamID,
				DataLimit: offset,
			})
			return false
		}
		return true
	}
	if f.FinBit {
		s.finSent = true
	}
	return s.dataForWriting != nil
}

func (s *sendStream) maybeGetRetransmission(maxBytes protocol.ByteCount) (wire.StreamFrameInterface, bool /* has more retransmissions */) {
	f := s.retransmissionQueue[0]
	fmt.Println("VIDEO: retransmission len: ", f.DataLen())
	newFrame, needsSplit := f.MaybeSplitOffFrame(maxBytes, s.version)
	if needsSplit {
		return newFrame, true
	}
	s.retransmissionQueue = s.retransmissionQueue[1:]
	return f, len(s.retransmissionQueue) > 0
}

func (s *sendStream) hasData() bool {
	s.mutex.Lock()
	hasData := len(s.dataForWriting) > 0
	s.mutex.Unlock()
	return hasData
}

func (s *sendStream) getDataForWriting(f *wire.StreamFrame, maxBytes protocol.ByteCount) {
	if s.dataForWriting == nil {
		f.FinBit = s.finishedWriting && !s.finSent
		return
	}

	maxBytes = utils.MinByteCount(maxBytes, s.flowController.SendWindowSize())
	if maxBytes == 0 {
		return
	}

	if protocol.ByteCount(len(s.dataForWriting)) > maxBytes {
		f.Data = f.Data[:maxBytes]
		copy(f.Data, s.dataForWriting)
		s.dataForWriting = s.dataForWriting[maxBytes:]
	} else {
		f.Data = f.Data[:len(s.dataForWriting)]
		copy(f.Data, s.dataForWriting)
		s.dataForWriting = nil
		s.signalWrite()
	}
	s.writeOffset += f.DataLen()
	s.flowController.AddBytesSent(f.DataLen())
	f.FinBit = s.finishedWriting && s.dataForWriting == nil && !s.finSent
}

func (s *sendStream) frameAcked(f wire.Frame) {
	f.(wire.StreamFrameInterface).PutBack()

	s.mutex.Lock()
	s.numOutstandingFrames--
	s.unreliableMustAckedNum--
	if s.numOutstandingFrames < 0 {
		panic("numOutStandingFrames negative")
	}
	newlyCompleted := s.isNewlyCompleted()
	s.mutex.Unlock()

	if newlyCompleted {
		s.sender.onStreamCompleted(s.streamID)
	}
}

func (s *sendStream) isNewlyCompleted() bool {
	var completed bool
	if s.unreliable {
		completed = (s.finSent || s.canceledWrite) && s.unreliableMustAckedNum == 0  && len(s.retransmissionQueue) == 0 //ackを受け取った時にfinsentを送信した後
		if completed {
			fmt.Println("VIDEO: send_stream.go: unreliable stream received ack of fin frame")
		}
	} else {
		completed = (s.finSent || s.canceledWrite) && s.numOutstandingFrames == 0 && len(s.retransmissionQueue) == 0
	}
	if completed && !s.completed {
		s.completed = true
		return true
	}
	return false
}

func (s *sendStream) queueRetransmission(f wire.Frame) {
	// stFrame, ok := f.(wire.StreamFrameInterface)
	// if s.unreliable &&
	// fmt.Println("unreliable retransmission queueing")

	switch sf := f.(type) {
	case *wire.StreamFrame:
		sf.DataLenPresent = true
		s.mutex.Lock()
		// fmt.Printf("add STREAM FRAME to retransmission queue id: %v\n", s.StreamID())
		s.retransmissionQueue = append(s.retransmissionQueue, sf)
		s.numOutstandingFrames--
		s.unreliableMustAckedNum--
		if s.numOutstandingFrames < 0 || s.unreliableMustAckedNum < 0{
			panic("numOutStandingFrames negative")
		}
		s.mutex.Unlock()

		s.sender.onHasStreamData(s.streamID)
		if sf.FinBit {
			fmt.Println("VIDEO: retransmit FIN STREAM")
		}
	case *wire.UnreliableStreamFrame:
		sf.DataLenPresent = true
		s.mutex.Lock()
		// fmt.Printf("add STREAM FRAME to retransmission queue id: %v\n", s.StreamID())
		s.retransmissionQueue = append(s.retransmissionQueue, sf)
		s.numOutstandingFrames--
		s.unreliableMustAckedNum--
		if s.numOutstandingFrames < 0 || s.unreliableMustAckedNum < 0{
			panic("numOutStandingFrames negative")
		}
		s.mutex.Unlock()

		s.sender.onHasStreamData(s.streamID)
		if sf.FinBit {
			fmt.Println("VIDEO: retransmit FIN STREAM")
		}
	}
}

func (s *sendStream) Close() error {
	s.mutex.Lock()
	if s.canceledWrite {
		s.mutex.Unlock()
		return fmt.Errorf("Close called for canceled stream %d", s.streamID)
	}
	s.ctxCancel()
	s.finishedWriting = true
	s.mutex.Unlock()

	s.sender.onHasStreamData(s.streamID) // need to send the FIN, must be called without holding the mutex
	return nil
}

func (s *sendStream) CancelWrite(errorCode protocol.ApplicationErrorCode) {
	s.cancelWriteImpl(errorCode, fmt.Errorf("Write on stream %d canceled with error code %d", s.streamID, errorCode))

}

// must be called after locking the mutex
func (s *sendStream) cancelWriteImpl(errorCode protocol.ApplicationErrorCode, writeErr error) {
	s.mutex.Lock()
	if s.canceledWrite {
		s.mutex.Unlock()
		return
	}
	s.ctxCancel()
	s.canceledWrite = true
	s.cancelWriteErr = writeErr
	newlyCompleted := s.isNewlyCompleted()
	s.mutex.Unlock()

	s.signalWrite()
	s.sender.queueControlFrame(&wire.ResetStreamFrame{
		StreamID:   s.streamID,
		ByteOffset: s.writeOffset,
		ErrorCode:  errorCode,
	})
	if newlyCompleted {
		s.sender.onStreamCompleted(s.streamID)
	}
}

func (s *sendStream) handleMaxStreamDataFrame(frame *wire.MaxStreamDataFrame) {
	s.mutex.Lock()
	hasStreamData := s.dataForWriting != nil
	s.mutex.Unlock()

	s.flowController.UpdateSendWindow(frame.ByteOffset)
	if hasStreamData {
		s.sender.onHasStreamData(s.streamID)
	}
}

func (s *sendStream) handleStopSendingFrame(frame *wire.StopSendingFrame) {
	writeErr := streamCanceledError{
		errorCode: frame.ErrorCode,
		error:     fmt.Errorf("stream %d was reset with error code %d", s.streamID, frame.ErrorCode),
	}
	s.cancelWriteImpl(frame.ErrorCode, writeErr)
}

func (s *sendStream) Context() context.Context {
	return s.ctx
}

func (s *sendStream) SetWriteDeadline(t time.Time) error {
	s.mutex.Lock()
	s.deadline = t
	s.mutex.Unlock()
	s.signalWrite()
	return nil
}

// CloseForShutdown closes a stream abruptly.
// It makes Write unblock (and return the error) immediately.
// The peer will NOT be informed about this: the stream is closed without sending a FIN or RST.
func (s *sendStream) closeForShutdown(err error) {
	s.mutex.Lock()
	s.ctxCancel()
	s.closedForShutdown = true
	s.closeForShutdownErr = err
	s.mutex.Unlock()
	s.signalWrite()
}

// signalWrite performs a non-blocking send on the writeChan
func (s *sendStream) signalWrite() {
	select {
	case s.writeChan <- struct{}{}:
	default:
	}
}
