package rdpgfx

// h264Frame holds a decoded H.264 frame in BGRA pixel format.
type h264Frame struct {
	Data          []byte // BGRA pixel data, 4 bytes per pixel
	Width, Height int
}

// h264Decoder decodes H.264 Annex B bitstream data into BGRA frames.
type h264Decoder interface {
	// Decode decodes H.264 NAL units and returns a decoded frame.
	// Returns nil frame (no error) when the decoder needs more input data.
	Decode(h264Data []byte) (*h264Frame, error)
	// Close releases all resources held by the decoder.
	Close()
}
