package wire

type StreamFrameInterface interface {
	Frame
	// Write(b *bytes.Buffer, version protocol.VersionNumber) error
}
