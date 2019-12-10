package wire

import (
	"bytes"
	"errors"
	"io"

	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/qerr"
	"github.com/lucas-clemente/quic-go/internal/utils"
)

type UnreliableStreamFrame struct {
	*StreamFrame
}

func parseUnreliableStreamFrame(r *bytes.Reader, _ protocol.VersionNumber) (*UnreliableStreamFrame, error) {
	typeByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	hasOffset := typeByte&0x4 > 0
	fin := typeByte&0x1 > 0
	hasDataLen := typeByte&0x2 > 0

	streamID, err := utils.ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	var offset uint64
	if hasOffset {
		offset, err = utils.ReadVarInt(r)
		if err != nil {
			return nil, err
		}
	}

	var dataLen uint64
	if hasDataLen {
		var err error
		dataLen, err = utils.ReadVarInt(r)
		if err != nil {
			return nil, err
		}
	} else {
		// The rest of the packet is data
		dataLen = uint64(r.Len())
	}

	var frame *StreamFrame
	if dataLen < protocol.MinStreamFrameBufferSize {
		frame = &StreamFrame{Data: make([]byte, dataLen)}
	} else {
		frame = GetStreamFrame()
		// The STREAM frame can't be larger than the StreamFrame we obtained from the buffer,
		// since those StreamFrames have a buffer length of the maximum packet size.
		if dataLen > uint64(cap(frame.Data)) {
			return nil, io.EOF
		}
		frame.Data = frame.Data[:dataLen]
	}

	frame.StreamID = protocol.StreamID(streamID)
	frame.Offset = protocol.ByteCount(offset)
	frame.FinBit = fin
	frame.DataLenPresent = hasDataLen

	if dataLen != 0 {
		if _, err := io.ReadFull(r, frame.Data); err != nil {
			return nil, err
		}
	}
	if frame.Offset+frame.DataLen() > protocol.MaxByteCount {
		return nil, qerr.Error(qerr.FrameEncodingError, "stream data overflows maximum offset")
	}
	return &UnreliableStreamFrame{frame}, nil
}

// パケット構築後、送信時に呼ばれる
func (f *UnreliableStreamFrame) Write(b *bytes.Buffer, version protocol.VersionNumber) error {
	if len(f.Data) == 0 && !f.FinBit {
		return errors.New("StreamFrame: attempting to write empty frame without FIN")
	}

	typeByte := byte(0x20)
	if f.FinBit {
		typeByte ^= 0x1
	}
	hasOffset := f.Offset != 0
	if f.DataLenPresent {
		typeByte ^= 0x2
	}
	if hasOffset {
		typeByte ^= 0x4
	}
	b.WriteByte(typeByte)
	utils.WriteVarInt(b, uint64(f.StreamID))
	if hasOffset {
		utils.WriteVarInt(b, uint64(f.Offset))
	}
	if f.DataLenPresent {
		utils.WriteVarInt(b, uint64(f.DataLen()))
	}
	b.Write(f.Data)
	return nil
}
