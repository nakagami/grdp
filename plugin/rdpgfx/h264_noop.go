//go:build !h264

package rdpgfx

// newH264Decoder returns nil when built without the "h264" build tag.
// AVC codecs will be disabled in RDPGFX capability negotiation.
func newH264Decoder() h264Decoder {
	return nil
}

// newH264DecoderSW returns nil when built without the "h264" build tag.
func newH264DecoderSW() h264Decoder {
	return nil
}

// newH264DecoderWithWatchdog returns nil when built without the "h264" build tag.
func newH264DecoderWithWatchdog(_ chan<- struct{}) h264Decoder {
	return nil
}

// newH264DecoderSWWithWatchdog returns nil when built without the "h264" build tag.
func newH264DecoderSWWithWatchdog(_ chan<- struct{}) h264Decoder {
	return nil
}



// h264PacketHasIDR always returns false when built without the "h264" build
// tag; the aux decoder is nil in that case so this path is never reached.
func h264PacketHasIDR(_ []byte) bool {
	return false
}

// combineAVC444BGRA is a no-op stub for builds without the "h264" build tag.
func combineAVC444BGRA(y1 []byte, _ int, y2 []byte, _ int, u2 []byte, _ int, _ bool, _, _ int) ([]byte, bool) {
	return nil, false
}
