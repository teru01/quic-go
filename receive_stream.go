package quic

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lucas-clemente/quic-go/internal/flowcontrol"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
)

type receiveStreamI interface {
	ReceiveStream

	handleStreamFrame(wire.StreamFrameInterface) error
	handleResetStreamFrame(*wire.ResetStreamFrame) error
	closeForShutdown(error)
	getWindowUpdate() protocol.ByteCount
}

type receiveStream struct {
	mutex sync.Mutex

	streamID protocol.StreamID

	sender streamSender

	frameQueue  *frameSorter
	readOffset  protocol.ByteCount
	finalOffset protocol.ByteCount

	currentFrame       []byte
	currentFrameDone   func()
	currentFrameIsLast bool // is the currentFrame the last frame on this stream
	readPosInFrame     int

	closeForShutdownErr error
	cancelReadErr       error
	resetRemotelyErr    StreamError

	closedForShutdown bool // set when CloseForShutdown() is called
	finRead           bool // set once we read a frame with a FinBit
	canceledRead      bool // set when CancelRead() is called
	resetRemotely     bool // set when HandleResetStreamFrame() is called

	readChan chan struct{}
	deadline time.Time

	flowController flowcontrol.StreamFlowController
	version        protocol.VersionNumber
}

var _ ReceiveStream = &receiveStream{}
var _ receiveStreamI = &receiveStream{}

func newReceiveStream(
	streamID protocol.StreamID,
	sender streamSender,
	flowController flowcontrol.StreamFlowController,
	version protocol.VersionNumber,
) *receiveStream {
	return &receiveStream{
		streamID:       streamID,
		sender:         sender,
		flowController: flowController,
		frameQueue:     newFrameSorter(),
		readChan:       make(chan struct{}, 1),
		finalOffset:    protocol.MaxByteCount,
		version:        version,
	}
}

func (s *receiveStream) StreamID() protocol.StreamID {
	return s.streamID
}

// Read implements io.Reader. It is not thread safe!
// この時にreceiveStreamにロス情報を持たせて上位にあげる
func (s *receiveStream) Read(p []byte) (int, error) {
	s.mutex.Lock()
	completed, n, err := s.readImpl(p)
	s.mutex.Unlock()

	if completed {
		s.sender.onStreamCompleted(s.streamID)
	}
	return n, err
}

type UnreliableReadResult struct{
	N int
	LossRange *utils.ByteIntervalList
}

// VIDEO: ロスした範囲を同時に返す
func (s *receiveStream) UnreliableRead(p []byte) (UnreliableReadResult, error) {
	s.mutex.Lock()
	completed, readResult, err := s.unreliableReadImpl(p)
	s.mutex.Unlock()

	if completed {
		s.sender.onStreamCompleted(s.streamID)
	}
	return readResult, err
}

func (s *receiveStream) unreliableReadImpl(p []byte) (bool /*stream completed */, UnreliableReadResult, error) {
	return true, UnreliableReadResult{N: 0, LossRange: nil}, nil
}

func (s *receiveStream) readImpl(p []byte) (bool /*stream completed */, int, error) {
	fmt.Println("read impl start")
	if s.finRead {
		return false, 0, io.EOF
	}
	if s.canceledRead {
		return false, 0, s.cancelReadErr
	}
	if s.resetRemotely {
		return false, 0, s.resetRemotelyErr
	}
	if s.closedForShutdown {
		return false, 0, s.closeForShutdownErr
	}

	bytesRead := 0
	for bytesRead < len(p) {
		fmt.Println("read loop first loop")
		if s.currentFrame == nil || s.readPosInFrame >= len(s.currentFrame) {
			s.dequeueNextFrame()
		}
		if s.currentFrame == nil && bytesRead > 0 {
			return false, bytesRead, s.closeForShutdownErr
		}

		var deadlineTimer *utils.Timer
		for {
			fmt.Println("read loop second loop")
			// Stop waiting on errors
			if s.closedForShutdown {
				return false, bytesRead, s.closeForShutdownErr
			}
			if s.canceledRead {
				return false, bytesRead, s.cancelReadErr
			}
			if s.resetRemotely {
				return false, bytesRead, s.resetRemotelyErr
			}

			deadline := s.deadline
			if !deadline.IsZero() {
				if !time.Now().Before(deadline) {
					return false, bytesRead, errDeadline
				}
				if deadlineTimer == nil {
					deadlineTimer = utils.NewTimer()
				}
				deadlineTimer.Reset(deadline)
			}

			if s.currentFrame != nil || s.currentFrameIsLast {
				if s.currentFrame != nil {
					fmt.Println("VIDEO: s.currentFrame")
				} else {
					fmt.Println("VIDEO: last")
				}
				break
			}

			s.mutex.Unlock()
			fmt.Println("read loop second loop before select")
			if deadline.IsZero() {
				fmt.Println("deadline zero")
				<-s.readChan
			} else {
				fmt.Println("deadline non zero")
				select {
				case <-s.readChan:
				case <-deadlineTimer.Chan():
					fmt.Println("timer chan")
					deadlineTimer.SetRead()
				}
			}
			fmt.Println("read loop second loop after select")
			s.mutex.Lock()
			if s.currentFrame == nil {
				s.dequeueNextFrame()
				fmt.Println("VIDEO: dequeued next frame")
			}
			fmt.Println("read loop second loop end")
		}

		if bytesRead > len(p) {
			return false, bytesRead, fmt.Errorf("BUG: bytesRead (%d) > len(p) (%d) in stream.Read", bytesRead, len(p))
		}
		if s.readPosInFrame > len(s.currentFrame) {
			return false, bytesRead, fmt.Errorf("BUG: readPosInFrame (%d) > frame.DataLen (%d) in stream.Read", s.readPosInFrame, len(s.currentFrame))
		}

		s.mutex.Unlock()

		m := copy(p[bytesRead:], s.currentFrame[s.readPosInFrame:])
		s.readPosInFrame += m
		bytesRead += m
		s.readOffset += protocol.ByteCount(m)

		s.mutex.Lock()
		// when a RESET_STREAM was received, the was already informed about the final byteOffset for this stream
		if !s.resetRemotely {
			s.flowController.AddBytesRead(protocol.ByteCount(m))
		}
		fmt.Println("read loop last")
		if !s.currentFrameIsLast {
			continue
		}
		// if s.sender.isUnreliableStream(s.StreamID()) ||
		// 	!s.sender.isUnreliableStream(s.StreamID()) && s.readPosInFrame >= len(s.currentFrame){
		// 		fmt.Println("recv stream end")
		// 		s.finRead = true
		// 		return true, bytesRead, io.EOF
		// }
		fmt.Printf("VIDEO: pos %v, len %v\n", s.readPosInFrame, len(s.currentFrame))
		if s.readPosInFrame >= len(s.currentFrame){ //currentFrameを最後までコピーした
			fmt.Println("recv stream end")
			s.finRead = true
			return true, bytesRead, io.EOF
		}

		// if s.readPosInFrame >= len(s.currentFrame) && s.currentFrameIsLast {
		// 	s.finRead = true
		// 	return true, bytesRead, io.EOF
		// }
	}
	return false, bytesRead, nil
}

func (s *receiveStream) dequeueNextFrame() {
	var offset protocol.ByteCount
	// We're done with the last frame. Release the buffer.
	if s.currentFrameDone != nil {
		s.currentFrameDone()
	}
	offset, s.currentFrame, s.currentFrameDone = s.frameQueue.Pop()
	// if s.sender.isUnreliableStream(s.StreamID()) {
	// 	fmt.Println("VIDEO: unreliable stream")
	// 	s.currentFrameIsLast = s.finalOffset != protocol.MaxByteCount
	// } else {
	// 	fmt.Println("VIDEO: reliable stream")
	// 	s.currentFrameIsLast = offset+protocol.ByteCount(len(s.currentFrame)) >= s.finalOffset
	// }
	s.currentFrameIsLast = offset+protocol.ByteCount(len(s.currentFrame)) >= s.finalOffset

	fmt.Printf("%v %v %v\n", offset, protocol.ByteCount(len(s.currentFrame)), s.finalOffset)
	// if s.sender.isUnreliableStream(s.StreamID()) {
	// 	//
	// } else {
	// 	s.currentFrameIsLast = offset+protocol.ByteCount(len(s.currentFrame)) >= s.finalOffset
	// }
	fmt.Println("islast: ", s.currentFrameIsLast)
	s.readPosInFrame = 0
}

func (s *receiveStream) CancelRead(errorCode protocol.ApplicationErrorCode) {
	s.mutex.Lock()
	completed := s.cancelReadImpl(errorCode)
	s.mutex.Unlock()

	if completed {
		s.flowController.Abandon()
		s.sender.onStreamCompleted(s.streamID)
	}
}

func (s *receiveStream) cancelReadImpl(errorCode protocol.ApplicationErrorCode) bool /* completed */ {
	if s.finRead || s.canceledRead || s.resetRemotely {
		return false
	}
	s.canceledRead = true
	s.cancelReadErr = fmt.Errorf("Read on stream %d canceled with error code %d", s.streamID, errorCode)
	s.signalRead()
	s.sender.queueControlFrame(&wire.StopSendingFrame{
		StreamID:  s.streamID,
		ErrorCode: errorCode,
	})
	// We're done with this stream if the final offset was already received.
	return s.finalOffset != protocol.MaxByteCount
}

func (s *receiveStream) handleStreamFrame(frame wire.StreamFrameInterface) error {
	s.mutex.Lock()
	completed, err := s.handleStreamFrameImpl(frame)
	s.mutex.Unlock()

	if completed {
		s.flowController.Abandon()
		s.sender.onStreamCompleted(s.streamID)
	}
	return err
}

func (s *receiveStream) handleStreamFrameImpl(frame wire.StreamFrameInterface) (bool /* completed */, error) {
	maxOffset := frame.GetOffset() + frame.GetDataLen()
	if err := s.flowController.UpdateHighestReceived(maxOffset, frame.GetFinBit()); err != nil {
		return false, err
	}
	var newlyRcvdFinalOffset bool
	if frame.GetFinBit() {
		newlyRcvdFinalOffset = s.finalOffset == protocol.MaxByteCount
		fmt.Println("VIDEO: get fin bit offset: ", maxOffset)  // これが最初に呼ばれるのが原因
		s.finalOffset = maxOffset
	}
	if s.canceledRead {
		return newlyRcvdFinalOffset, nil
	}
	if err := s.frameQueue.Push(frame.GetData(), frame.GetOffset(), frame.PutBack); err != nil {
		return false, err
	}
	switch frame.(type) {
	case *wire.StreamFrame:
		s.sender.setUnreliableMap(s.StreamID(), false)
	case *wire.UnreliableStreamFrame:
		s.sender.setUnreliableMap(s.StreamID(), true)
	}
	s.signalRead()
	return false, nil
}

func (s *receiveStream) handleResetStreamFrame(frame *wire.ResetStreamFrame) error {
	s.mutex.Lock()
	completed, err := s.handleResetStreamFrameImpl(frame)
	s.mutex.Unlock()

	if completed {
		s.flowController.Abandon()
		s.sender.onStreamCompleted(s.streamID)
	}
	return err
}

func (s *receiveStream) handleResetStreamFrameImpl(frame *wire.ResetStreamFrame) (bool /*completed */, error) {
	if s.closedForShutdown {
		return false, nil
	}
	if err := s.flowController.UpdateHighestReceived(frame.ByteOffset, true); err != nil {
		return false, err
	}
	newlyRcvdFinalOffset := s.finalOffset == protocol.MaxByteCount
	fmt.Println("reset stream: byteoffset ", frame.ByteOffset)
	s.finalOffset = frame.ByteOffset

	// ignore duplicate RESET_STREAM frames for this stream (after checking their final offset)
	if s.resetRemotely {
		return false, nil
	}
	s.resetRemotely = true
	s.resetRemotelyErr = streamCanceledError{
		errorCode: frame.ErrorCode,
		error:     fmt.Errorf("stream %d was reset with error code %d", s.streamID, frame.ErrorCode),
	}
	s.signalRead()
	return newlyRcvdFinalOffset, nil
}

// func (s *receiveStream) CloseRemote(offset protocol.ByteCount) {
// 	s.handleStreamFrame(&wire.StreamFrame{FinBit: true, Offset: offset})
// }

func (s *receiveStream) SetReadDeadline(t time.Time) error {
	s.mutex.Lock()
	s.deadline = t
	s.mutex.Unlock()
	s.signalRead()
	return nil
}

// CloseForShutdown closes a stream abruptly.
// It makes Read unblock (and return the error) immediately.
// The peer will NOT be informed about this: the stream is closed without sending a FIN or RESET.
func (s *receiveStream) closeForShutdown(err error) {
	s.mutex.Lock()
	s.closedForShutdown = true
	s.closeForShutdownErr = err
	s.mutex.Unlock()
	s.signalRead()
}

func (s *receiveStream) getWindowUpdate() protocol.ByteCount {
	return s.flowController.GetWindowUpdate()
}

// signalRead performs a non-blocking send on the readChan
func (s *receiveStream) signalRead() {
	select {
	case s.readChan <- struct{}{}:
	default:
	}
}
