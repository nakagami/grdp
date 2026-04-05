package rdpgfx

// AVC420 / AVC444 bitmap stream parsing (MS-RDPEGFX 2.2.4.6 / 2.2.4.7).

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"
)

type avcRect struct {
	left, top, right, bottom uint16
}

type avcQuantQuality struct {
	qp          uint8
	quality     uint8
	progressive bool
}

type avc420Stream struct {
	regions      []avcRect
	quantQuality []avcQuantQuality
	h264Data     []byte
}

// parseAVC420Stream parses RDPGFX_AVC420_BITMAP_STREAM.
func parseAVC420Stream(data []byte) (*avc420Stream, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("avc420 stream too short (%d bytes)", len(data))
	}

	numRegions := binary.LittleEndian.Uint32(data[:4])
	if numRegions > 65536 {
		return nil, fmt.Errorf("avc420: too many regions: %d", numRegions)
	}

	// 4 bytes header + 8 bytes per region rect + 2 bytes per quant/quality
	metaSize := 4 + int(numRegions)*10
	if metaSize > len(data) {
		return nil, fmt.Errorf("avc420: metadata truncated (need %d, have %d)", metaSize, len(data))
	}

	off := 4
	regions := make([]avcRect, numRegions)
	for i := uint32(0); i < numRegions; i++ {
		regions[i] = avcRect{
			left:   binary.LittleEndian.Uint16(data[off:]),
			top:    binary.LittleEndian.Uint16(data[off+2:]),
			right:  binary.LittleEndian.Uint16(data[off+4:]),
			bottom: binary.LittleEndian.Uint16(data[off+6:]),
		}
		off += 8
	}

	qq := make([]avcQuantQuality, numRegions)
	for i := uint32(0); i < numRegions; i++ {
		qp := data[off]
		qual := data[off+1]
		qq[i] = avcQuantQuality{
			qp:          qp & 0x3F,
			quality:     qual,
			progressive: (qp & 0x40) != 0,
		}
		off += 2
	}

	return &avc420Stream{
		regions:      regions,
		quantQuality: qq,
		h264Data:     data[metaSize:],
	}, nil
}

// parseAVC444Stream parses RDPGFX_AVC444_BITMAP_STREAM.
// Returns the main AVC420 stream and the LC (luma-chroma) field.
//
//	LC=0: both streams present; we decode the main (YUV420) stream.
//	LC=1: main stream only.
//	LC=2: auxiliary only (chroma upgrade); returns nil stream.
func parseAVC444Stream(data []byte) (*avc420Stream, uint8, error) {
	if len(data) < 4 {
		return nil, 0, fmt.Errorf("avc444 stream too short")
	}

	cbField := binary.LittleEndian.Uint32(data[:4])
	lc := uint8((cbField >> 30) & 0x03)
	cbStream1 := int(cbField & 0x3FFFFFFF)
	rest := data[4:]

	switch lc {
	case 0: // Both streams; decode main
		if cbStream1 > len(rest) {
			return nil, lc, fmt.Errorf("avc444: stream1 size %d exceeds data %d", cbStream1, len(rest))
		}
		s, err := parseAVC420Stream(rest[:cbStream1])
		return s, lc, err
	case 1: // Main stream only
		streamData := rest
		if cbStream1 > 0 && cbStream1 <= len(rest) {
			streamData = rest[:cbStream1]
		}
		s, err := parseAVC420Stream(streamData)
		return s, lc, err
	case 2: // Auxiliary only (chroma upgrade) — not yet supported
		return nil, lc, nil
	default:
		return nil, lc, fmt.Errorf("avc444: invalid LC=%d", lc)
	}
}

// decodeAVC420 decodes AVC420 bitmap data to BGRA pixels.
func (g *GfxHandler) decodeAVC420(data []byte, destW, destH int) []byte {
	if g.h264dec == nil {
		return nil
	}
	stream, err := parseAVC420Stream(data)
	if err != nil {
		slog.Warn("RDPGFX: AVC420 parse error", "err", err)
		return nil
	}
	if len(stream.h264Data) == 0 {
		return nil
	}
	frame, err := g.h264dec.Decode(stream.h264Data)
	if err != nil {
		slog.Warn("RDPGFX: H.264 decode error", "err", err)
		return nil
	}
	if frame == nil {
		g.maybeRequestKeyframe()
		slog.Debug("RDPGFX: H.264 decode returned nil frame (buffering?)")
		return nil
	}
	g.keyframeRequested = false
	slog.Debug("RDPGFX: AVC420 decoded", "frameW", frame.Width, "frameH", frame.Height, "destW", destW, "destH", destH, "regions", len(stream.regions), "h264Len", len(stream.h264Data))
	return cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)
}

// decodeAVC444 decodes AVC444 bitmap data to BGRA pixels.
// Currently decodes the main YUV420 stream only (LC=0,1).
func (g *GfxHandler) decodeAVC444(data []byte, destW, destH int) []byte {
	if g.h264dec == nil {
		return nil
	}
	stream, lc, err := parseAVC444Stream(data)
	if err != nil {
		slog.Warn("RDPGFX: AVC444 parse error", "err", err)
		return nil
	}
	if stream == nil {
		if lc == 2 {
			slog.Debug("RDPGFX: AVC444 LC=2 (chroma upgrade) skipped")
		}
		return nil
	}
	if len(stream.h264Data) == 0 {
		return nil
	}
	frame, err := g.h264dec.Decode(stream.h264Data)
	if err != nil {
		slog.Warn("RDPGFX: H.264 decode error (AVC444)", "err", err)
		return nil
	}
	if frame == nil {
		g.maybeRequestKeyframe()
		return nil
	}
	g.keyframeRequested = false
	slog.Debug("RDPGFX: AVC444 decoded", "frameW", frame.Width, "frameH", frame.Height,
		"destW", destW, "destH", destH, "h264Len", len(stream.h264Data))
	return cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)
}

// maybeRequestKeyframe calls onKeyframeNeeded (if set) the first time the
// H.264 decoder reports it is waiting for an IDR after a reset, and again
// every 3 seconds while still waiting.  This handles the case where the first
// refresh request goes unanswered (e.g. after an HW→SW decoder switch where
// the server never retransmits an IDR in time).
func (g *GfxHandler) maybeRequestKeyframe() {
	if g.onKeyframeNeeded == nil {
		return
	}
	if !g.h264dec.NeedsKeyframe() {
		return
	}
	// Rate-limit: allow re-request if the previous one was more than 3 seconds ago.
	if g.keyframeRequested && time.Since(g.lastKeyframeRequest) < 3*time.Second {
		return
	}
	g.keyframeRequested = true
	g.lastKeyframeRequest = time.Now()
	go g.onKeyframeNeeded()
}

// cropBGRA crops or pads BGRA pixel data to the target dimensions.
func cropBGRA(src []byte, srcW, srcH, dstW, dstH int) []byte {
	if srcW == dstW && srcH == dstH {
		return src
	}
	out := make([]byte, dstW*dstH*4)
	copyW := dstW
	if srcW < copyW {
		copyW = srcW
	}
	copyH := dstH
	if srcH < copyH {
		copyH = srcH
	}
	srcStride := srcW * 4
	dstStride := dstW * 4
	rowBytes := copyW * 4
	for y := 0; y < copyH; y++ {
		copy(out[y*dstStride:y*dstStride+rowBytes], src[y*srcStride:y*srcStride+rowBytes])
	}
	return out
}
