package wire

import "github.com/lucas-clemente/quic-go/internal/protocol"

type StreamFrameInterface interface {
	Frame
	GetDataLen() protocol.ByteCount
	GetStreamId() protocol.StreamID
	GetOffset() protocol.ByteCount
	GetFinBit() bool
	GetData() []byte
	PutBack()
	// Write(b *bytes.Buffer, version protocol.VersionNumber) error
}
