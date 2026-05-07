package rdpgfx

import "time"

// h264BrokenReason describes why a decoder became unrecoverable.
type h264BrokenReason int

const (
	h264BrokenReasonNone h264BrokenReason = iota
	h264BrokenReasonInitFailure
	h264BrokenReasonHWStall
	h264BrokenReasonNoIDR
)

func (r h264BrokenReason) String() string {
	switch r {
	case h264BrokenReasonInitFailure:
		return "init-failure"
	case h264BrokenReasonHWStall:
		return "hw-stall"
	case h264BrokenReasonNoIDR:
		return "no-idr"
	default:
		return "none"
	}
}

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
	Y, U, V                   []byte
	YStride, UStride, VStride int
	Width, Height             int
	FullRange                 bool // true when the source used full-range (JPEG/PC) YUV
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
	// (IDR) before it can produce output, or is otherwise stalling and
	// needs the server to send a refresh.  This happens after a decoder
	// reset; the caller should request a refresh from the server.
	// For H/W decoders this also returns true when output has stalled
	// beyond a threshold (hwStallKeyframeThreshold).
	NeedsKeyframe() bool
	// NeedsIDR reports whether the decoder is explicitly waiting for an
	// IDR frame after a reset.  Unlike NeedsKeyframe it does NOT include
	// the H/W stall heuristic, so it is safe to poll even for decoders
	// that only receive frames infrequently (e.g. h264dec2 for LC=2).
	NeedsIDR() bool
	// IsBroken reports whether the decoder is permanently unrecoverable.
	// When true, the application should reconnect the RDP session to
	// create a fresh decoder.
	IsBroken() bool
	// BrokenReason reports why the decoder became unrecoverable.
	BrokenReason() h264BrokenReason
	// ForceBroken marks the decoder unrecoverable for the given reason.
	ForceBroken(reason h264BrokenReason)
	// HardResetCount returns the number of hard resets performed so far.
	// Callers can use this to detect a new reset and clear rate-limit state.
	HardResetCount() int
	// LastReceiveTime returns the wall-clock time of the most recent Decode()
	// call, or a zero Time if Decode has never been called.  The caller uses
	// this to distinguish a genuinely idle server (no packets → zero or old
	// LastReceiveTime) from a HW decoder stall (packets arriving but no
	// decoded frames produced).
	LastReceiveTime() time.Time
	// Close releases all resources held by the decoder.
	Close()
}
