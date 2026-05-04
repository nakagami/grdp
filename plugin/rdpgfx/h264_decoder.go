package rdpgfx

// h264Frame holds a decoded H.264 frame in BGRA pixel format.
type h264Frame struct {
	Data          []byte // BGRA pixel data, 4 bytes per pixel
	Width, Height int
}

// h264FrameI420 holds a decoded H.264 frame in planar I420 (YUV420P) format.
// SDL2 can render I420 natively via hardware-accelerated YUV→RGB shaders using
// a PIXELFORMAT_IYUV texture, eliminating CPU-side colour conversion.
// Plane slices borrow ring-buffer memory; the caller must copy all slices
// before the next Decode call.
type h264FrameI420 struct {
	Y, U, V             []byte
	YStride, UStride, VStride int
	Width, Height       int
}

// i420Decoder is an optional interface that an h264Decoder may implement to
// produce I420 output alongside the normal BGRA frame.  Callers detect support
// via a type assertion.
type i420Decoder interface {
	// DecodeWithI420 decodes H.264 NAL data and returns both a BGRA frame
	// (same as Decode, used internally for the surface backing store) and an
	// optional I420 frame for GPU-accelerated rendering.  The I420 frame is
	// nil when the pixel format is not directly convertible (e.g. swscale
	// paths); callers must fall back to the BGRA frame in that case.
	DecodeWithI420(h264Data []byte) (*h264Frame, *h264FrameI420, error)
}

// h264Decoder decodes H.264 Annex B bitstream data into BGRA frames.
type h264Decoder interface {
	// Decode decodes H.264 NAL units and returns a decoded frame.
	// Returns nil frame (no error) when the decoder needs more input data.
	Decode(h264Data []byte) (*h264Frame, error)
	// NeedsKeyframe reports whether the decoder is waiting for a keyframe
	// (IDR) before it can produce output.  This happens after a decoder
	// reset; the caller should request a refresh from the server.
	NeedsKeyframe() bool
	// IsBroken reports whether the decoder is permanently unrecoverable.
	// When true, the application should reconnect the RDP session to
	// create a fresh decoder.
	IsBroken() bool
	// HardResetCount returns the number of hard resets performed so far.
	// Callers can use this to detect a new reset and clear rate-limit state.
	HardResetCount() int
	// Close releases all resources held by the decoder.
	Close()
}
