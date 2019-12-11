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
	MaybeSplitOffFrame(protocol.ByteCount, protocol.VersionNumber) (*StreamFrame, bool)
	DataLen() protocol.ByteCount
	MaxDataLen(protocol.ByteCount, protocol.VersionNumber) protocol.ByteCount
}
