package wire

import (
	"sync"

	"github.com/lucas-clemente/quic-go/internal/protocol"
)

var pool sync.Pool

// poolはフレーム生成に使われる。ACK済みのパケットはプールに戻して再利用される。GCの負荷を下げるため
func init() {
	pool.New = func() interface{} {
		return &StreamFrame{
			Data:     make([]byte, 0, protocol.MaxReceivePacketSize),
			fromPool: true,
		}
	}
}

func GetStreamFrame() *StreamFrame {
	f := pool.Get().(*StreamFrame)
	return f
}

func putStreamFrame(f *StreamFrame) {
	if !f.fromPool {
		return
	}
	if protocol.ByteCount(cap(f.Data)) != protocol.MaxReceivePacketSize {
		panic("wire.PutStreamFrame called with packet of wrong size!")
	}
	pool.Put(f)
}
