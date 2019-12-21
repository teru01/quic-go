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

	readChan    chan struct{}
	finReadChan chan struct{}
	deadline    time.Time

	flowController flowcontrol.StreamFlowController
	version        protocol.VersionNumber

	dataPayloadOffset protocol.ByteCount
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
		finReadChan:    make(chan struct{}, 1),
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

type ByteRange struct {
	Start protocol.ByteCount
	End protocol.ByteCount
}

type UnreliableReadResult struct {
	N         int
	LossRange []ByteRange /*VIDEO: ロスレンジのリスト。計算量改善したい*/
}

// VIDEO: ロスした範囲を同時に返す
func (s *receiveStream) UnreliableRead(p []byte) (*UnreliableReadResult, error) {
	if s.dataPayloadOffset == 0 { // そのストリームの初回のUnreliable Read
		s.dataPayloadOffset = s.frameQueue.readPos
	}
	s.mutex.Lock()
	result := UnreliableReadResult{N: 0, LossRange: make([]ByteRange, 0)}
	completed, readResult, err := s.unreliableReadImpl(p, &result)
	s.mutex.Unlock()

	if completed {
		s.sender.onStreamCompleted(s.streamID)
	}
	return readResult, err
}

// VIDEO データフレームのペイロード読み取りにのみ使われる
// FINが届いているならブロックしない
func (s *receiveStream) unreliableReadImpl(p []byte, result *UnreliableReadResult) (bool /*stream completed */, *UnreliableReadResult, error) {
	fmt.Println("unreliableread impl start")
	if s.finRead {
		fmt.Println("VIDEO: FINREAD")
		return false, result, io.EOF
	}
	if s.canceledRead {
		fmt.Println("VIDEO: CANCELREAD")
		return false, result, s.cancelReadErr
	}
	if s.resetRemotely {
		fmt.Println("VIDEO: RESETREMOTE")
		return false, result, s.resetRemotelyErr
	}
	if s.closedForShutdown {
		fmt.Println("VIDEO: CLOSEDFORSHUTDOWN")
		return false, result, s.closeForShutdownErr
	}

	bytesRead := 0
	for bytesRead < len(p) {
		if s.currentFrame == nil || s.readPosInFrame >= len(s.currentFrame) { // VIDEO: フレームを全てバッファにコピーできた
			select {
			case <- s.finReadChan:
				s.forceDequeNextFrame(result)
				s.signalFinRead()
			default:
				s.dequeueNextFrame()
			}
		}
		if s.currentFrame == nil && bytesRead > 0 {
			result.N = bytesRead
			fmt.Println("VIDEO: unreliable closeForShutdownErr")
			return false, result, s.closeForShutdownErr //VIDEO TODO fix
		}

		var deadlineTimer *utils.Timer
		for {
			// Stop waiting on errors
			if s.closedForShutdown {
				return false, &UnreliableReadResult{N: bytesRead, LossRange: nil}, s.closeForShutdownErr
			}
			if s.canceledRead {
				return false, &UnreliableReadResult{N: bytesRead, LossRange: nil}, s.cancelReadErr
			}
			if s.resetRemotely {
				return false, &UnreliableReadResult{N: bytesRead, LossRange: nil}, s.resetRemotelyErr
			}

			deadline := s.deadline
			if !deadline.IsZero() {
				if !time.Now().Before(deadline) {
					return false, &UnreliableReadResult{N: bytesRead, LossRange: nil}, errDeadline
				}
				if deadlineTimer == nil {
					deadlineTimer = utils.NewTimer()
				}
				deadlineTimer.Reset(deadline)
			}

			if s.currentFrame != nil || s.currentFrameIsLast {
				fmt.Println("VIDEO: unreliableReadImpl: break inner loop")
				break
			}
			s.mutex.Unlock()
			if deadline.IsZero() {
				fmt.Println("VIDEO: unreliableReadImpl: deadline zero") // 次のオフセットにフレームがセットされるのを待つ
				select {
				case <-s.readChan:
				case <-s.finReadChan:
					// FINが読まれたのでdequeueしてロスレンジを計算 null byte padding
					s.forceDequeNextFrame(result)
					s.signalFinRead()
				}
			} else {
				fmt.Println("VIDEO: unreliableReadImpl: deadline non zero")
				select {
				case <-s.readChan:
				case <-s.finReadChan:
					// FINが読まれたのでdequeueしてロスレンジを計算 null byte padding
					s.forceDequeNextFrame(result)
					s.signalFinRead()
				case <-deadlineTimer.Chan():
					deadlineTimer.SetRead()
				}
			}
			s.mutex.Lock()
			if s.currentFrame == nil {
				select {
				case <- s.finReadChan:
					s.forceDequeNextFrame(result)
					s.signalFinRead()
				default:
					s.dequeueNextFrame()
				}
			}
		}

		if bytesRead > len(p) {
			return false, &UnreliableReadResult{N: bytesRead, LossRange: nil}, fmt.Errorf("BUG: bytesRead (%d) > len(p) (%d) in stream.Read", bytesRead, len(p))
		}
		if s.readPosInFrame > len(s.currentFrame) {
			return false, &UnreliableReadResult{N: bytesRead, LossRange: nil}, fmt.Errorf("BUG: readPosInFrame (%d) > frame.DataLen (%d) in stream.Read", s.readPosInFrame, len(s.currentFrame))
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
		fmt.Printf("VIDEO: pos %v, len %v\n", s.readPosInFrame, len(s.currentFrame))
		if s.readPosInFrame >= len(s.currentFrame) { //currentFrameを最後までコピーした
			fmt.Println("recv stream end")
			s.finRead = true
			result.N = bytesRead
			return true, result, io.EOF // VIDDEO: TODO impl
		}
	}
	fmt.Printf("VIDEO: bytesread: %v, len(p): %v\n", bytesRead, len(p))
	result.N = bytesRead
	return false, result, nil // VIDDEO: TODO impl
}

func (s *receiveStream) readImpl(p []byte) (bool /*stream completed */, int, error) {
	if s.finRead {
		fmt.Println("VIDEO: FINREAD")
		return false, 0, io.EOF
	}
	if s.canceledRead {
		fmt.Println("VIDEO: CANCELREAD")
		return false, 0, s.cancelReadErr
	}
	if s.resetRemotely {
		fmt.Println("VIDEO: RESETREMOTE")
		return false, 0, s.resetRemotelyErr
	}
	if s.closedForShutdown {
		fmt.Println("VIDEO: CLOSEDFORSHUTDOWN")
		return false, 0, s.closeForShutdownErr
	}

	bytesRead := 0
	for bytesRead < len(p) {
		fmt.Printf("readpos: %v, len(frame) %v:\n", s.readPosInFrame, len(s.currentFrame))
		if s.currentFrame == nil || s.readPosInFrame >= len(s.currentFrame) {
			fmt.Println("VIDEO: first dequue")
			s.dequeueNextFrame()
		}
		if s.currentFrame == nil && bytesRead > 0 {
			fmt.Println("VIDEO: closeForShutdownErr: ", s.closeForShutdownErr)
			return false, bytesRead, s.closeForShutdownErr
		}

		var deadlineTimer *utils.Timer
		for {
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
				fmt.Println("VIDEO inner loop break")
				break
			}

			s.mutex.Unlock()
			if deadline.IsZero() {
				fmt.Println("deadline zero") // 次のオフセットにフレームがセットされるのを待つ
				<-s.readChan
			} else {
				fmt.Println("deadline non zero")
				select {
				case <-s.readChan:
				case <-deadlineTimer.Chan():
					deadlineTimer.SetRead()
				}
			}
			s.mutex.Lock()
			if s.currentFrame == nil {
				s.dequeueNextFrame()
			}
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
		if !s.currentFrameIsLast {
			continue
		}
		fmt.Printf("VIDEO: pos %v, len %v\n", s.readPosInFrame, len(s.currentFrame))
		if s.readPosInFrame >= len(s.currentFrame) { //currentFrameを最後までコピーした
			s.finRead = true
			return true, bytesRead, io.EOF
		}

	}
	fmt.Printf("VIDEO: bytesread: %v, len(p): %v\n", bytesRead, len(p))
	return false, bytesRead, nil
}

// VIDEO: If force is true, pop null frame and forward cursor
func (s *receiveStream) dequeueNextFrame() {
	var offset protocol.ByteCount
	// We're done with the last frame. Release the buffer.
	if s.currentFrameDone != nil {
		s.currentFrameDone()
	}
	offset, s.currentFrame, s.currentFrameDone = s.frameQueue.Pop()
	if s.StreamID() == 0 {
		if s.currentFrame == nil {
			fmt.Println("VIDEO: current frame is nil")
		}
		fmt.Println("VIDEO: pop frame: offset ", offset)
	}
	s.currentFrameIsLast = offset+protocol.ByteCount(len(s.currentFrame)) >= s.finalOffset

	fmt.Printf("%v %v %v\n", offset, protocol.ByteCount(len(s.currentFrame)), s.finalOffset)
	fmt.Println("islast: ", s.currentFrameIsLast)
	s.readPosInFrame = 0
}

// VIDEO 次のフレームが存在していなくてもnullbyteをpopする
func (s *receiveStream) forceDequeNextFrame(result *UnreliableReadResult) {
	fmt.Println("VIDEO: force dequeue next frame start")
	var offset protocol.ByteCount
	// We're done with the last frame. Release the buffer.
	if s.currentFrameDone != nil {
		s.currentFrameDone()
	}
	var isPaddingFragment bool
	offset, s.currentFrame, s.currentFrameDone, isPaddingFragment = s.frameQueue.ForcePop()
	if isPaddingFragment {
		fmt.Printf("VIDEO: force poped. append lossRange: %v-%v\n", offset, offset + protocol.ByteCount(len(s.currentFrame)))
		result.LossRange = append(result.LossRange, ByteRange{Start: offset - s.dataPayloadOffset, End: offset + protocol.ByteCount(len(s.currentFrame)) - s.dataPayloadOffset})
	}
	if s.StreamID() == 0 {
		if s.currentFrame == nil {
			fmt.Println("VIDEO: current frame is nil")
		}
		fmt.Println("VIDEO: Force pop frame: offset ", offset)
	}
	s.currentFrameIsLast = offset+protocol.ByteCount(len(s.currentFrame)) >= s.finalOffset

	fmt.Printf("%v %v %v\n", offset, protocol.ByteCount(len(s.currentFrame)), s.finalOffset)
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
		fmt.Println("VIDEO: get fin bit max offset: ", maxOffset) // これが最初に呼ばれるのが原因
		s.finalOffset = maxOffset
	}
	if s.canceledRead {
		return newlyRcvdFinalOffset, nil
	}
	err := s.frameQueue.Push(frame.GetData(), frame.GetOffset(), frame.PutBack)
	if err != nil {
		return false, err
	}
	
	if frame.GetFinBit() {
		s.frameQueue.setMaxOffset(frame.GetOffset())
		s.signalFinRead()
	}
	if s.StreamID() == 0 { // VIDEO: client initiated bidi stream
		fmt.Printf("VIDEO: push frame: offset: %v length: %v\n", frame.GetOffset(), len(frame.GetData()))
		if frame.GetFinBit() {
			fmt.Println("VIDEO: FIN offset: ", frame.GetOffset())
		}
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

func (s *receiveStream) signalFinRead() {
	select {
	case s.finReadChan <- struct{}{}:
	default:
	}
}
