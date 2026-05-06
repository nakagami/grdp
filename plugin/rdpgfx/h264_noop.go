//go:build !h264

package rdpgfx

// newH264Decoder returns nil when built without the "h264" build tag.
// AVC codecs will be disabled in RDPGFX capability negotiation.
func newH264Decoder() h264Decoder {
	return nil
}

// combineAVC444BGRA is a no-op stub for builds without the "h264" build tag.
func combineAVC444BGRA(y1 []byte, _ int, y2 []byte, _ int, u2 []byte, _ int, _ bool, _, _ int) ([]byte, bool) {
	return nil, false
}
