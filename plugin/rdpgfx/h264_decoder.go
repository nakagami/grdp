package rdpgfx

import "time"

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
	// CountSkippedPacket notifies the decoder that a packet arrived but was
	// discarded before reaching the codec (e.g. an AVC444 LC=2 chroma-upgrade
	// frame that the client does not yet support).  When the decoder is in a
	// post-hard-reset window waiting for a fresh IDR, the skipped packet is
	// counted toward hwPostResetStuckThreshold so that the stuck-detection
	// fires at the expected rate even when the server mostly sends non-luma
	// frames.  Returns true if the decoder was just marked broken as a result.
	CountSkippedPacket() bool
	// NudgeForLumaRefresh checks whether the decoder has not produced a luma
	// frame for at least threshold and, if so, marks it as wanting a server
	// refresh so that the next maybeRequestKeyframe() call will fire.
	// Returns true if the stall was detected and the flag was set.
	// No-op (returns false) if the decoder has not yet produced any frame or
	// is already waiting for a refresh.
	NudgeForLumaRefresh(threshold time.Duration) bool
	// Close releases all resources held by the decoder.
	Close()
}
