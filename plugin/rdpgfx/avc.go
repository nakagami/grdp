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

// regionHinter is an optional interface implemented by decoders that support
// region-aware YUV→BGRA conversion.  When setRegionHint is called immediately
// before Decode, the decoder only converts pixels within the specified dirty
// rectangles, skipping unchanged areas of the frame and reducing CPU cost for
// small updates such as scroll, cursor movement, or partial redraws.
type regionHinter interface {
	setRegionHint(regions []avcRect)
}

type avc420Stream struct {
	regions  []avcRect
	h264Data []byte
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

	// 4 bytes header + 10 bytes per region (8-byte rect + 2-byte quant/quality)
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

	return &avc420Stream{
		regions:  regions,
		h264Data: data[metaSize:],
	}, nil
}

// parseAVC444Stream parses RDPGFX_AVC444_BITMAP_STREAM.
// Returns the main AVC420 stream, the auxiliary AVC420 stream, and the LC
// (luma-chroma) field.
//
//	LC=0: both streams present; stream1 = main (YUV420), stream2 = chroma upgrade.
//	LC=1: main stream only; stream2 is nil.
//	LC=2: auxiliary only (chroma upgrade); stream1 is nil.
func parseAVC444Stream(data []byte) (stream1, stream2 *avc420Stream, lc uint8, err error) {
	if len(data) < 4 {
		return nil, nil, 0, fmt.Errorf("avc444 stream too short")
	}

	cbField := binary.LittleEndian.Uint32(data[:4])
	lc = uint8((cbField >> 30) & 0x03)
	cbStream1 := int(cbField & 0x3FFFFFFF)
	rest := data[4:]

	switch lc {
	case 0: // Both streams present
		if cbStream1 > len(rest) {
			return nil, nil, lc, fmt.Errorf("avc444: stream1 size %d exceeds data %d", cbStream1, len(rest))
		}
		stream1, err = parseAVC420Stream(rest[:cbStream1])
		if err != nil {
			return nil, nil, lc, err
		}
		if cbStream1 < len(rest) {
			stream2, err = parseAVC420Stream(rest[cbStream1:])
			if err != nil {
				slog.Debug("RDPGFX: AVC444 stream2 parse error (LC=0)", "err", err)
				stream2 = nil
				err = nil
			}
		}
		return stream1, stream2, lc, nil
	case 1: // Main stream only
		streamData := rest
		if cbStream1 > 0 && cbStream1 <= len(rest) {
			streamData = rest[:cbStream1]
		}
		stream1, err = parseAVC420Stream(streamData)
		return stream1, nil, lc, err
	case 2: // Auxiliary only (chroma upgrade)
		streamData := rest
		if cbStream1 > 0 && cbStream1 <= len(rest) {
			streamData = rest[:cbStream1]
		}
		stream2, err = parseAVC420Stream(streamData)
		return nil, stream2, lc, err
	default:
		return nil, nil, lc, fmt.Errorf("avc444: invalid LC=%d", lc)
	}
}

// avc444YPlane caches the tightly-packed luma plane (stride = Width) from the
// most recently decoded AVC444 main stream.  It is used to combine with the
// auxiliary chroma stream when LC=2 frames arrive.
type avc444YPlane struct {
	data      []byte // luma Y, tight-packed, stride = w
	u         []byte // Cb (U) plane from stream1, half-res, stride = (w+1)/2
	v         []byte // Cr (V) plane from stream1, half-res, stride = (w+1)/2
	stride    int    // = w
	uvStride  int    // = (w+1)/2
	w, h      int
	fullRange bool
	updatedAt time.Time // last time the cache was refreshed from a live main-stream decode
}

// avc444YStaleness is the maximum age of the Y-plane cache before LC=2
// combines are suppressed.  When the main decoder (h264dec) stalls, the
// Y-plane is frozen while incoming LC=2 frames carry fresh chroma — combining
// stale luma with fresh chroma produces wrong colours.  500 ms is well above
// the inter-frame interval at typical RDP frame rates (≥2 fps) yet much lower
// than the 7-second hard stall threshold, so normal operation is unaffected.
const avc444YStaleness = 500 * time.Millisecond

// isH264Keyframe returns true when data contains an IDR NAL unit (type 5),
// which marks the start of a new GOP (key frame).  The scan handles both
// 3-byte (00 00 01) and 4-byte (00 00 00 01) Annex-B start codes.
func isH264Keyframe(data []byte) bool {
	for i := 0; i+4 <= len(data); i++ {
		// Look for Annex-B start code: 00 00 01 or 00 00 00 01.
		if data[i] == 0x00 && data[i+1] == 0x00 {
			var nalByte byte
			if data[i+2] == 0x01 && i+3 < len(data) {
				nalByte = data[i+3]
				i += 2
			} else if data[i+2] == 0x00 && i+3 < len(data) && data[i+3] == 0x01 && i+4 < len(data) {
				nalByte = data[i+4]
				i += 3
			} else {
				continue
			}
			nalType := nalByte & 0x1F
			if nalType == 5 { // IDR slice
				return true
			}
		}
	}
	return false
}

// decodeAVC420 decodes AVC420 bitmap data to BGRA pixels and returns the
// decoded frame plus the dirty rectangle list reported in the AVC420 stream
// header (in decoded-frame coordinates).  When regions is non-empty callers
// can blit only those regions instead of the whole frame, which dramatically
// reduces per-frame copying for typical desktop video where most of the
// frame is unchanged from the previous frame.
// The pooled return value is true when the returned slice was acquired from
// bitmapBufPool; the caller must then call releaseBitmapBuf on it.
func (g *GfxHandler) decodeAVC420(data []byte, destX, destY, destW, destH int) ([]byte, []avcRect, bool) {
	// Parse the stream header once and reuse for both the raw-NAL callback and
	// the actual decode path, avoiding a redundant walk of the metadata.
	stream, parseErr := parseAVC420Stream(data)
	if g.onH264Raw != nil && parseErr == nil && len(stream.h264Data) > 0 {
		isKF := isH264Keyframe(stream.h264Data)
		nalData := make([]byte, len(stream.h264Data))
		copy(nalData, stream.h264Data)
		g.onH264Raw(destX, destY, destW, destH, isKF, nalData)
	}
	if g.h264dec == nil {
		return nil, nil, false
	}
	if parseErr != nil {
		slog.Warn("RDPGFX: AVC420 parse error", "err", parseErr)
		return nil, nil, false
	}
	if len(stream.h264Data) == 0 {
		return nil, nil, false
	}
	// For frames where only a small dirty area changed, pass region hints so
	// the decoder can skip converting pixels outside those rectangles.  This
	// is safe here because decodeAVC420 uses blitAndEmitAVCRegions (which only
	// reads dirty pixels) when shouldUseAVCRegions returns true.
	if rh, ok := g.h264dec.(regionHinter); ok &&
		len(stream.regions) > 0 && shouldUseAVCRegions(stream.regions, destW, destH) {
		rh.setRegionHint(stream.regions)
	}
	frame, err := g.h264dec.Decode(stream.h264Data)
	if err != nil {
		slog.Warn("RDPGFX: H.264 decode error", "err", err)
		return nil, nil, false
	}
	if frame == nil {
		g.maybeRequestKeyframe()
		g.maybeNotifyDecoderBroken()
		slog.Debug("RDPGFX: H.264 decode returned nil frame (buffering?)")
		return nil, nil, false
	}
	slog.Debug("RDPGFX: AVC420 decoded", "frameW", frame.Width, "frameH", frame.Height, "destW", destW, "destH", destH, "regions", len(stream.regions), "h264Len", len(stream.h264Data))
	g.noteSuccessfulDecode()
	decoded, pooled := cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)
	return decoded, stream.regions, pooled
}

// decodeAVC444 decodes AVC444 bitmap data to BGRA pixels.
// LC=0 and LC=1 decode the main YUV420 stream and cache the luma plane for
// potential LC=2 combine.  LC=2 combines the cached luma with the auxiliary
// chroma stream decoded by the secondary decoder.
// The pooled return value is true when the returned slice was acquired from
// bitmapBufPool; the caller must then call releaseBitmapBuf on it.
func (g *GfxHandler) decodeAVC444(data []byte, destX, destY, destW, destH int) ([]byte, []avcRect, bool) {
	// Parse the stream header once and reuse for both the raw-NAL callback and
	// the actual decode path, avoiding a redundant walk of the metadata.
	stream1, stream2, lc, parseErr := parseAVC444Stream(data)
	if g.onH264Raw != nil && parseErr == nil && stream1 != nil && len(stream1.h264Data) > 0 {
		isKF := isH264Keyframe(stream1.h264Data)
		nalData := make([]byte, len(stream1.h264Data))
		copy(nalData, stream1.h264Data)
		g.onH264Raw(destX, destY, destW, destH, isKF, nalData)
	}
	if g.h264dec == nil {
		return nil, nil, false
	}
	if parseErr != nil {
		slog.Warn("RDPGFX: AVC444 parse error", "err", parseErr)
		return nil, nil, false
	}
	if lc == 2 {
		return g.decodeAVC444LC2(stream2, destW, destH)
	}
	if stream1 == nil || len(stream1.h264Data) == 0 {
		return nil, nil, false
	}

	// Pass region hints so the decoder skips converting pixels outside the
	// dirty rectangles.  Safe here because decodeAVC444 also uses
	// blitAndEmitAVCRegions when shouldUseAVCRegions returns true.
	if rh, ok := g.h264dec.(regionHinter); ok &&
		len(stream1.regions) > 0 && shouldUseAVCRegions(stream1.regions, destW, destH) {
		rh.setRegionHint(stream1.regions)
	}

	var frame *h264Frame
	var i420out *h264FrameI420
	var err error
	isIDR := g.h264dec2 != nil && isH264Keyframe(stream1.h264Data)
	if isIDR {
		// Reset per-GOP diagnostic flags so the LC=0 IDR and LC=2 combine
		// after this IDR are sampled again for colour diagnostics.
		g.lc2SampleLogged = false
		g.lc2PFrameSampleLogged = false
		g.lc0SampleLogged = false
	}
	if g.h264dec2 != nil {
		// Cache luma for future LC=2 combine.
		if i420dec, ok := g.h264dec.(i420Decoder); ok {
			frame, i420out, err = i420dec.DecodeWithI420(stream1.h264Data)
			if err != nil {
				slog.Warn("RDPGFX: H.264 decode error (AVC444)", "err", err)
				return nil, nil, false
			}
			if i420out != nil {
				g.updateAVC444YCache(i420out)
				if isIDR {
					// Snapshot the IDR luma separately.  When a standalone
					// LC=2 packet carries a stream2 IDR, the chroma data
					// belongs to this GOP's first frame, so we must combine
					// it with the IDR luma — not with a later P-frame's luma
					// that has since overwritten avc444YPlane.
					g.copyAVC444YToIDRCache()
				}
			}
		} else {
			frame, err = g.h264dec.Decode(stream1.h264Data)
		}
	} else {
		frame, err = g.h264dec.Decode(stream1.h264Data)
	}
	if err != nil {
		slog.Warn("RDPGFX: H.264 decode error (AVC444)", "err", err)
		return nil, nil, false
	}
	if frame == nil {
		if i420out == nil {
			g.maybeRequestKeyframe()
			g.maybeNotifyDecoderBroken()
			return nil, nil, false
		}
		// I420 fast path: HW decoder returned planar I420 instead of BGRA.
		// Convert to BGRA using BT.709 (AVC444 standard encoding) so the
		// BGRA rendering path can continue normally.
		bgra, _ := i420ToBGRA(i420out)
		if bgra == nil {
			return nil, nil, false
		}
		frame = &h264Frame{Data: bgra, Width: i420out.Width, Height: i420out.Height}
	}
	if !g.lc0SampleLogged && isIDR {
		g.lc0SampleLogged = true
		bgraData := frame.Data
		w, h := frame.Width, frame.Height
		for _, p := range [][2]int{{960, 400}, {480, 400}, {1440, 400}, {960, 600}, {100, 100}} {
			px, py := p[0], p[1]
			if px >= w || py >= h {
				continue
			}
			off := (py*w + px) * 4
			if off+3 < len(bgraData) {
				var rawY, rawU, rawV byte
				if i420out != nil && py < i420out.Height && px < i420out.Width {
					rawY = i420out.Y[py*i420out.YStride+px]
					rawU = i420out.U[(py/2)*i420out.UStride+(px/2)]
					rawV = i420out.V[(py/2)*i420out.VStride+(px/2)]
				}
				slog.Debug("H.264: pixel sample (LC=0 IDR frame)",
					"x", px, "y", py,
					"rawY", rawY, "rawU", rawU, "rawV", rawV,
					"fullRange", i420out != nil && i420out.FullRange,
					"B", bgraData[off], "G", bgraData[off+1], "R", bgraData[off+2])
			}
		}
	}
	slog.Debug("RDPGFX: AVC444 decoded", "frameW", frame.Width, "frameH", frame.Height,
		"destW", destW, "destH", destH, "h264Len", len(stream1.h264Data))
	g.noteSuccessfulDecode()
	decoded, pooled := cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)

	// For LC=0 frames prime h264dec2 with the auxiliary (chroma-upgrade) stream2
	// so that subsequent standalone LC=2 P-frames can be decoded.  The IDR frame
	// for the auxiliary H.264 sequence is always carried in an LC=0 stream2.
	// Always call primeAuxDecoder even when h264dec2 is nil: the function
	// gates recreation on an IDR being present in stream2.
	if lc == 0 && stream2 != nil && len(stream2.h264Data) > 0 {
		g.primeAuxDecoder(stream2.h264Data)
	}

	return decoded, stream1.regions, pooled
}

// decodeAVC420WithI420 decodes AVC420 bitmap data, returning BGRA pixels for
// the surface backing store and, when the underlying decoder supports I420
// output, an optional h264FrameI420 for GPU-accelerated IYUV texture upload.
// i420 is nil when I420 extraction is unsupported or the frame dimensions are
// smaller than destW×destH.  Callers must fall back to BGRA rendering when
// i420 is nil.
func (g *GfxHandler) decodeAVC420WithI420(data []byte, destX, destY, destW, destH int) (decoded []byte, i420 *h264FrameI420, regions []avcRect, pooled bool) {
	stream, err := parseAVC420Stream(data)
	if err != nil {
		slog.Warn("RDPGFX: AVC420 parse error", "err", err)
		return
	}
	if g.onH264Raw != nil && len(stream.h264Data) > 0 {
		isKF := isH264Keyframe(stream.h264Data)
		nalData := make([]byte, len(stream.h264Data))
		copy(nalData, stream.h264Data)
		g.onH264Raw(destX, destY, destW, destH, isKF, nalData)
	}
	if g.h264dec == nil || len(stream.h264Data) == 0 {
		return
	}
	var frame *h264Frame
	i420dec, hasI420 := g.h264dec.(i420Decoder)
	if hasI420 {
		var i420out *h264FrameI420
		frame, i420out, err = i420dec.DecodeWithI420(stream.h264Data)
		if err != nil {
			slog.Warn("RDPGFX: H.264 decode error", "err", err)
			return
		}
		if i420out != nil && i420out.Width >= destW && i420out.Height >= destH {
			i420 = i420out
		}
	} else {
		frame, err = g.h264dec.Decode(stream.h264Data)
		if err != nil {
			slog.Warn("RDPGFX: H.264 decode error", "err", err)
			return
		}
	}
	// I420 fast path: frame is nil but i420 is non-nil — decoder produced output
	// via the direct NV12/YUV420P copy path.  Still counts as a successful decode.
	if frame == nil && i420 == nil {
		g.maybeRequestKeyframe()
		g.maybeNotifyDecoderBroken()
		slog.Debug("RDPGFX: H.264 decode returned nil frame (buffering?)")
		return
	}
	g.noteSuccessfulDecode()
	if frame != nil {
		slog.Debug("RDPGFX: AVC420 decoded (WithI420)", "frameW", frame.Width, "frameH", frame.Height,
			"destW", destW, "destH", destH, "hasI420", i420 != nil,
			"regions", len(stream.regions), "h264Len", len(stream.h264Data))
		decoded, pooled = cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)
	}
	regions = stream.regions
	return
}

// decodeAVC420WithNV12 decodes AVC420 bitmap data, returning native NV12
// planes when the underlying decoder produces NV12 (typically VideoToolbox).
// If NV12 is unavailable, decoded may contain a BGRA fallback frame.
func (g *GfxHandler) decodeAVC420WithNV12(data []byte, destX, destY, destW, destH int) (decoded []byte, nv12 *h264FrameNV12, regions []avcRect, pooled bool) {
	stream, err := parseAVC420Stream(data)
	if err != nil {
		slog.Warn("RDPGFX: AVC420 parse error", "err", err)
		return
	}
	if g.onH264Raw != nil && len(stream.h264Data) > 0 {
		isKF := isH264Keyframe(stream.h264Data)
		nalData := make([]byte, len(stream.h264Data))
		copy(nalData, stream.h264Data)
		g.onH264Raw(destX, destY, destW, destH, isKF, nalData)
	}
	if g.h264dec == nil || len(stream.h264Data) == 0 {
		return
	}
	var frame *h264Frame
	nv12dec, hasNV12 := g.h264dec.(nv12Decoder)
	if hasNV12 {
		var nv12out *h264FrameNV12
		frame, nv12out, err = nv12dec.DecodeWithNV12(stream.h264Data)
		if err != nil {
			slog.Warn("RDPGFX: H.264 decode error", "err", err)
			return
		}
		if nv12out != nil && nv12out.Width >= destW && nv12out.Height >= destH {
			nv12 = nv12out
		}
	} else {
		frame, err = g.h264dec.Decode(stream.h264Data)
		if err != nil {
			slog.Warn("RDPGFX: H.264 decode error", "err", err)
			return
		}
	}
	if frame == nil && nv12 == nil {
		g.maybeRequestKeyframe()
		g.maybeNotifyDecoderBroken()
		slog.Debug("RDPGFX: H.264 decode returned nil frame (buffering?)")
		return
	}
	g.noteSuccessfulDecode()
	if frame != nil {
		slog.Debug("RDPGFX: AVC420 decoded (WithNV12)", "frameW", frame.Width, "frameH", frame.Height,
			"destW", destW, "destH", destH, "hasNV12", nv12 != nil,
			"regions", len(stream.regions), "h264Len", len(stream.h264Data))
		decoded, pooled = cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)
	}
	regions = stream.regions
	return
}

// decodeAVC444WithI420 decodes AVC444 bitmap data, returning BGRA pixels and
// an optional I420 frame.  LC=0 and LC=1 decode the main stream and cache the
// luma plane.  LC=2 decodes the auxiliary chroma stream and combines it with
// the cached luma to produce BGRA; i420 is nil for LC=2 frames (GPU path falls
// back to BGRA).
func (g *GfxHandler) decodeAVC444WithI420(data []byte, destX, destY, destW, destH int) (decoded []byte, i420 *h264FrameI420, regions []avcRect, pooled bool) {
	stream1, stream2, lc, err := parseAVC444Stream(data)
	if g.onH264Raw != nil && stream1 != nil && len(stream1.h264Data) > 0 {
		isKF := isH264Keyframe(stream1.h264Data)
		nalData := make([]byte, len(stream1.h264Data))
		copy(nalData, stream1.h264Data)
		g.onH264Raw(destX, destY, destW, destH, isKF, nalData)
	}
	if err != nil {
		slog.Warn("RDPGFX: AVC444 parse error", "err", err)
		return
	}
	if lc == 2 {
		decoded, regions, pooled = g.decodeAVC444LC2(stream2, destW, destH)
		return
	}
	if g.h264dec == nil || stream1 == nil || len(stream1.h264Data) == 0 {
		return
	}
	var frame *h264Frame
	i420dec, hasI420 := g.h264dec.(i420Decoder)
	if hasI420 {
		var i420out *h264FrameI420
		frame, i420out, err = i420dec.DecodeWithI420(stream1.h264Data)
		if err != nil {
			slog.Warn("RDPGFX: H.264 decode error (AVC444)", "err", err)
			return
		}
		if i420out != nil {
			if g.h264dec2 != nil {
				g.updateAVC444YCache(i420out)
			}
			if i420out.Width >= destW && i420out.Height >= destH {
				i420 = i420out
			}
		}
	} else {
		frame, err = g.h264dec.Decode(stream1.h264Data)
		if err != nil {
			slog.Warn("RDPGFX: H.264 decode error (AVC444)", "err", err)
			return
		}
	}
	// I420 fast path: frame is nil but i420 is non-nil — decoder produced output
	// via the direct NV12/YUV420P copy path.  Still counts as a successful decode.
	if frame == nil && i420 == nil {
		g.maybeRequestKeyframe()
		g.maybeNotifyDecoderBroken()
		return
	}
	g.noteSuccessfulDecode()
	if frame != nil {
		slog.Debug("RDPGFX: AVC444 decoded (WithI420)", "frameW", frame.Width, "frameH", frame.Height,
			"destW", destW, "destH", destH, "hasI420", i420 != nil, "h264Len", len(stream1.h264Data))
		decoded, pooled = cropBGRA(frame.Data, frame.Width, frame.Height, destW, destH)
		regions = stream1.regions
	}

	// For LC=0 frames prime h264dec2 with the auxiliary (chroma-upgrade) stream2.
	// Always call even when h264dec2 is nil: primeAuxDecoder gates recreation on
	// an IDR being present in stream2.
	if lc == 0 && stream2 != nil && len(stream2.h264Data) > 0 {
		g.primeAuxDecoder(stream2.h264Data)
	}

	return
}

// updateAVC444YCache copies the Y, U, and V planes from stream1's i420 into
// g.avc444YPlane for use when combining with an LC=2 auxiliary chroma frame.
// The U/V planes are stored half-res (stride = (w+1)/2) and provide the B2/B3
// chroma values (even column, even row positions) that stream2 does not cover.
func (g *GfxHandler) updateAVC444YCache(i420 *h264FrameI420) {
	w, h := i420.Width, i420.Height
	uvStride := (w + 1) / 2
	uvH := (h + 1) / 2
	neededY := w * h
	neededUV := uvStride * uvH
	if cap(g.avc444YPlane.data) < neededY {
		g.avc444YPlane.data = make([]byte, neededY)
	} else {
		g.avc444YPlane.data = g.avc444YPlane.data[:neededY]
	}
	if cap(g.avc444YPlane.u) < neededUV {
		g.avc444YPlane.u = make([]byte, neededUV)
	} else {
		g.avc444YPlane.u = g.avc444YPlane.u[:neededUV]
	}
	if cap(g.avc444YPlane.v) < neededUV {
		g.avc444YPlane.v = make([]byte, neededUV)
	} else {
		g.avc444YPlane.v = g.avc444YPlane.v[:neededUV]
	}
	// i420 planes are already tight-packed (strides == width/height from extractI420fromSrc).
	copy(g.avc444YPlane.data, i420.Y)
	copy(g.avc444YPlane.u, i420.U)
	copy(g.avc444YPlane.v, i420.V)
	g.avc444YPlane.stride = w
	g.avc444YPlane.uvStride = uvStride
	g.avc444YPlane.w = w
	g.avc444YPlane.h = h
	g.avc444YPlane.fullRange = i420.FullRange
	g.avc444YPlane.updatedAt = time.Now()
}

// copyAVC444YToIDRCache copies the current avc444YPlane content into
// avc444IDRYPlane.  Called immediately after updating avc444YPlane from a
// stream1 IDR decode, so the IDR luma snapshot stays separate from any
// subsequent P-frame luma updates.
func (g *GfxHandler) copyAVC444YToIDRCache() {
	src := &g.avc444YPlane
	dst := &g.avc444IDRYPlane
	if cap(dst.data) < len(src.data) {
		dst.data = make([]byte, len(src.data))
	} else {
		dst.data = dst.data[:len(src.data)]
	}
	if cap(dst.u) < len(src.u) {
		dst.u = make([]byte, len(src.u))
	} else {
		dst.u = dst.u[:len(src.u)]
	}
	if cap(dst.v) < len(src.v) {
		dst.v = make([]byte, len(src.v))
	} else {
		dst.v = dst.v[:len(src.v)]
	}
	copy(dst.data, src.data)
	copy(dst.u, src.u)
	copy(dst.v, src.v)
	dst.stride = src.stride
	dst.uvStride = src.uvStride
	dst.w = src.w
	dst.h = src.h
	dst.fullRange = src.fullRange
	dst.updatedAt = src.updatedAt
}

// combineAVC444v2BGRA implements the AVC444v2 chroma reconstruction defined in
// [MS-RDPEGFX 3.3.8.3.3] ("YUV420p Stream Combination for YUV444v2 mode").
//
// Stream2 encodes the missing chroma positions that stream1's 4:2:0 quantiser
// discards, split across three "Bx areas" of the auxiliary I420 frame:
//
//	B4/B5 — stream2 Y plane, each row:
//	  bytes [0,   w/2)  = Cb at all odd-x columns  (U444[2k+1, y]  for k=0..w/2-1)
//	  bytes [w/2, w)    = Cr at all odd-x columns  (V444[2k+1, y]  for k=0..w/2-1)
//
//	B6/B7 — stream2 U plane, each half-height row j:
//	  bytes [0,    w/4) = Cb at even-x multiples of 4  (U444[4k,   2j+1])
//	  bytes [w/4,  w/2) = Cr at even-x multiples of 4  (V444[4k,   2j+1])
//
//	B8/B9 — stream2 V plane, each half-height row j:
//	  bytes [0,    w/4) = Cb at even-x offset-2 cols   (U444[4k+2, 2j+1])
//	  bytes [w/4,  w/2) = Cr at even-x offset-2 cols   (V444[4k+2, 2j+1])
//
// Positions not covered by stream2 (even-x, even-y) use stream1's half-res
// B2/B3 chroma values from the cached cachedU/cachedV planes.
//
// Parameters:
//
//	yPlane/yStride       – luma Y from stream1, tight-packed (stride=w)
//	cachedU/cachedV      – Cb/Cr from stream1, half-res (stride=uvStride=(w+1)/2)
//	i420aux              – I420 output from decoding stream2
//	fullRange            – true for PC-range [0-255], false for video [16-235]
func combineAVC444v2BGRA(
	yPlane []byte, yStride int,
	cachedU, cachedV []byte, uvStride int,
	i420aux *h264FrameI420,
	fullRange bool,
	w, h int,
) (out []byte, pooled bool) {
	if len(yPlane) == 0 || len(cachedU) == 0 || len(cachedV) == 0 || w <= 0 || h <= 0 {
		return nil, false
	}
	if i420aux == nil || len(i420aux.Y) == 0 || len(i420aux.U) == 0 || len(i420aux.V) == 0 {
		return nil, false
	}
	out = acquireBitmapBuf(w * h * 4)
	halfW := w / 2
	quarterW := w / 4
	auxYStride := i420aux.YStride
	auxUStride := i420aux.UStride
	auxVStride := i420aux.VStride
	for row := 0; row < h; row++ {
		yRow := yPlane[row*yStride:]
		outRow := out[row*w*4:]
		auxYRow := i420aux.Y[row*auxYStride:]
		uvRow := row >> 1
		for col := 0; col < w; col++ {
			Y := yRow[col]
			var Cb, Cr byte
			if col&1 == 1 {
				// Odd column: B4/B5 — both even and odd rows.
				k := col >> 1
				Cb = auxYRow[k]
				Cr = auxYRow[halfW+k]
			} else if row&1 == 0 {
				// Even column, even row: B2/B3 from stream1 cached chroma.
				Cb = cachedU[uvRow*uvStride+(col>>1)]
				Cr = cachedV[uvRow*uvStride+(col>>1)]
			} else {
				// Even column, odd row: B6-B9.
				k := col >> 2
				if col&2 == 0 {
					// col % 4 == 0: B6/B7 from stream2 U plane.
					Cb = i420aux.U[uvRow*auxUStride+k]
					Cr = i420aux.U[uvRow*auxUStride+quarterW+k]
				} else {
					// col % 4 == 2: B8/B9 from stream2 V plane.
					Cb = i420aux.V[uvRow*auxVStride+k]
					Cr = i420aux.V[uvRow*auxVStride+quarterW+k]
				}
			}
			avc444bt709BGRA(Y, Cb, Cr, fullRange, outRow[col*4:])
		}
	}
	return out, true
}

// i420ToBGRA converts a planar I420 frame to a packed BGRA buffer using BT.709
// coefficients (matching AVC444 content encoding). Used when the I420 fast path
// is active and a BGRA output is required by the rendering path.
func i420ToBGRA(src *h264FrameI420) ([]byte, bool) {
	if src == nil || src.Width <= 0 || src.Height <= 0 {
		return nil, false
	}
	w, h := src.Width, src.Height
	out := acquireBitmapBuf(w * h * 4)
	for row := 0; row < h; row++ {
		uvRow := row >> 1
		for col := 0; col < w; col++ {
			Y := src.Y[row*src.YStride+col]
			U := src.U[uvRow*src.UStride+(col>>1)]
			V := src.V[uvRow*src.VStride+(col>>1)]
			avc444bt709BGRA(Y, U, V, src.FullRange, out[(row*w+col)*4:])
		}
	}
	return out, true
}

// avc444bt709BGRA converts one YCbCr pixel to BGRA using BT.709 coefficients,
// matching FreeRDP's general_YUV444ToBGRX implementation.
// Windows AVC444v2 content is encoded in BT.709; using BT.601 here was the
// cause of red color bleeding on LC=2 chroma-upgrade frames.
// Cb and Cr are raw (0-255); the function subtracts 128 internally.
//
// Full range  (Y∈[0,255]):   R = Y + 1.5748*(Cr-128)   ≈ (256y + 403v) >> 8
// Limited range (Y∈[16,235]): R = 1.164*(Y-16) + 1.793*(Cr-128) ≈ (298c + 459v) >> 8
func avc444bt709BGRA(Y, Cb, Cr byte, fullRange bool, dst []byte) {
	u := int(Cb) - 128
	v := int(Cr) - 128
	var r, g, b int
	if fullRange {
		y := int(Y)
		r = (256*y + 403*v + 128) >> 8
		g = (256*y - 48*u - 120*v + 128) >> 8
		b = (256*y + 475*u + 128) >> 8
	} else {
		c := int(Y) - 16
		r = (298*c + 459*v + 128) >> 8
		g = (298*c - 55*u - 136*v + 128) >> 8
		b = (298*c + 541*u + 128) >> 8
	}
	dst[0] = clampByte(b)
	dst[1] = clampByte(g)
	dst[2] = clampByte(r)
	dst[3] = 255
}

// clampByte clamps an integer to [0, 255].
func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// auxiliary H.264 sequence.  The IDR frame for the chroma-upgrade stream is
// always carried in the LC=0 stream2; subsequent LC=2 standalone frames are
// P-frames in the same sequence.  Decoding output is discarded — the only
// purpose is to advance h264dec2's internal state past the IDR/SPS/PPS so it
// can decode the subsequent LC=2 P-frames without waiting forever for a
// keyframe that already passed.
func (g *GfxHandler) primeAuxDecoder(h264Data []byte) {
	// Only process IDR frames.  Standalone LC=2 P-frames are decoded
	// exclusively by decodeAVC444LC2 and form their own H.264 reference chain
	// (IDR → P1 → P2 → …).  If primeAuxDecoder also fed LC=0's stream2
	// P-frames to h264dec2, the DPB would be advanced ahead of where the
	// standalone LC=2 stream expects its reference, causing the first P-frame
	// to be decoded against the wrong reference and producing Cb=0 (green tint).
	if !h264PacketHasIDR(h264Data) {
		return
	}
	if g.h264dec2 == nil {
		// Aux decoder was torn down after a VT stall.  Recreate it only when
		// the incoming stream2 carries an IDR so the new VT session starts with
		// a clean reference frame.  This avoids the rapid create/destroy cycle
		// that stresses macOS VideoToolbox and destabilises the main decoder.
		slog.Debug("H.264: recreating aux decoder on stream2 IDR")
		g.h264dec2 = newH264DecoderSW()
		g.stopAuxDecoderBrokenTimer() // LC=0 IDR arrived; cancel recovery timer
		// Fall through to prime the freshly-created decoder with this IDR.
	}
	// If the aux decoder was already broken (e.g. from a prior pre-flight stall),
	// tear it down and wait for the next IDR rather than immediately creating
	// a new VT session.
	if g.h264dec2.IsBroken() {
		slog.Debug("H.264: aux decoder broken, waiting for IDR to recreate")
		g.h264dec2.Close()
		g.h264dec2 = nil
		g.startAuxDecoderBrokenTimer()
		return
	}
	i420dec, ok := g.h264dec2.(i420Decoder)
	if !ok {
		return
	}
	_, _, err := i420dec.DecodeWithI420(h264Data)
	if err != nil {
		slog.Debug("RDPGFX: AVC444 aux prime error", "err", err)
	}
	// The pre-flight stall detector inside DecodeWithI420 can set broken=true
	// and return nil,nil without an error (broken state invisible to caller).
	// Check IsBroken() after the call to catch this case.
	if g.h264dec2.IsBroken() {
		slog.Debug("H.264: aux decoder broken after prime, waiting for IDR to recreate")
		g.h264dec2.Close()
		g.h264dec2 = nil
		g.startAuxDecoderBrokenTimer()
	}
}

// decodeAVC444LC2 decodes an AVC444 LC=2 chroma-upgrade frame.
// It decodes stream2 via the auxiliary decoder, then combines the cached luma
// (Y plane) with the auxiliary chroma (Y2 = U/Cb channel, U2 = V/Cr channel)
// to produce a BGRA frame.
func (g *GfxHandler) decodeAVC444LC2(stream2 *avc420Stream, destW, destH int) (decoded []byte, regions []avcRect, pooled bool) {
	// Record LC=2 arrival unconditionally so maybeRenegotiateCapabilities can
	// distinguish an active-LC=2-only server from a truly idle server.
	g.lastLC2RecvTime.Store(time.Now().UnixNano())
	if g.h264dec2 == nil {
		slog.Debug("RDPGFX: AVC444 LC=2 skipped (no aux decoder)")
		return
	}
	if stream2 == nil || len(stream2.h264Data) == 0 {
		slog.Debug("RDPGFX: AVC444 LC=2 skipped (empty aux stream)")
		return
	}
	// If the main decoder is broken (e.g. HW stall or no IDR received), trigger
	// soft reset so it can recover even when only LC=2 (chroma-only) frames are
	// arriving and the LC=0/1 decode path never gets called.
	if g.h264dec != nil && g.h264dec.IsBroken() {
		g.maybeNotifyDecoderBroken()
		return
	}
	if g.avc444YPlane.w == 0 {
		slog.Debug("RDPGFX: AVC444 LC=2 skipped (no cached luma)")
		g.maybeRequestKeyframe()
		return
	}
	// Skip the combine when the Y cache is stale: the main decoder is likely
	// stalling (VideoToolbox null frames).  Combining old luma with fresh chroma
	// produces visible colour artefacts.  We suppress LC=2 output until h264dec
	// delivers a fresh frame and refreshes the cache.
	if !g.avc444YPlane.updatedAt.IsZero() && time.Since(g.avc444YPlane.updatedAt) > avc444YStaleness {
		age := time.Since(g.avc444YPlane.updatedAt).Round(time.Millisecond)
		slog.Debug("RDPGFX: AVC444 LC=2 skipped (Y cache stale, main decoder likely stalling)",
			"age", age)
		// During a VideoToolbox stall h264dec.NeedsKeyframe() is false (the
		// decoder has not been reset) so maybeRequestKeyframe() returns early.
		// Request a keyframe directly here, reusing the shared rate-limiter, so
		// the server delivers a fresh IDR that can help break the VT stall.
		const keyframeRequestInterval = 2 * time.Second
		if g.onKeyframeRequest != nil && time.Since(g.lastKeyframeRequest) >= keyframeRequestInterval {
			g.lastKeyframeRequest = time.Now()
			go g.onKeyframeRequest()
		}
		return
	}
	i420dec, ok := g.h264dec2.(i420Decoder)
	if !ok {
		slog.Debug("RDPGFX: AVC444 LC=2 skipped (aux decoder lacks I420 support)")
		return
	}
	_, i420aux, err := i420dec.DecodeWithI420(stream2.h264Data)
	if err != nil {
		slog.Warn("RDPGFX: AVC444 LC=2 aux decode error", "err", err)
		if g.h264dec2.IsBroken() {
			g.h264dec2.Close()
			g.h264dec2 = nil
			g.startAuxDecoderBrokenTimer()
		}
		return
	}
	if i420aux == nil {
		slog.Debug("RDPGFX: AVC444 LC=2 aux decode buffering")
		// The pre-flight stall detector inside Decode() may have set broken=true
		// and returned nil without an error.  Detect and tear down here; the
		// decoder will be recreated by primeAuxDecoder when the next stream2
		// IDR arrives, avoiding a rapid VT session create/destroy cycle.
		if g.h264dec2 != nil && g.h264dec2.IsBroken() {
			slog.Debug("H.264: aux decoder broken during LC=2 decode, waiting for IDR to recreate")
			g.h264dec2.Close()
			g.h264dec2 = nil
			// Do NOT call maybeRequestKeyframe() here: ForceRefresh only delivers
			// LC=1 luma IDR, not a stream2/chroma IDR.  h264dec2 will be re-primed
			// naturally when the next LC=0 frame arrives via primeAuxDecoder.
			// The aux decoder broken timer will escalate to caps renegotiation if
			// no LC=0 IDR arrives within auxDecoderBrokenTimeout.
			g.startAuxDecoderBrokenTimer()
		}
		return
	}
	// Select the luma plane for the combine.  When stream2 carries an IDR its
	// chroma data corresponds to the GOP-boundary frame, not to the latest
	// P-frame.  Using avc444IDRYPlane (a snapshot of the luma at the moment
	// stream1's IDR was decoded) avoids combining mismatched luma/chroma planes
	// and eliminates the transient green tint that appears at GOP boundaries
	// when the server delivers the stream2 IDR as a standalone LC=2 packet.
	// Fall back to avc444YPlane when no IDR snapshot is available (e.g. the
	// VideoToolbox pipeline delayed the IDR output past the P-frame boundary).
	stream2IsIDR := isH264Keyframe(stream2.h264Data)
	yp := &g.avc444YPlane
	if stream2IsIDR && g.avc444IDRYPlane.w > 0 {
		yp = &g.avc444IDRYPlane
		slog.Debug("RDPGFX: AVC444 LC=2 IDR combine using IDR luma snapshot")
	}
	w, h := yp.w, yp.h
	if i420aux.Width < w || i420aux.Height < h {
		slog.Debug("RDPGFX: AVC444 LC=2 aux frame too small",
			"auxW", i420aux.Width, "auxH", i420aux.Height, "lumaW", w, "lumaH", h)
		return
	}
	combined, _ := combineAVC444v2BGRA(
		yp.data, yp.stride,
		yp.u, yp.v, yp.uvStride,
		i420aux,
		yp.fullRange,
		w, h,
	)
	if combined == nil {
		return
	}
	// lc2Sample logs the actual Cb/Cr values used by combineAVC444v2BGRA for
	// position (px,py), which depend on the B-area that pixel falls into.
	halfW := w / 2
	quarterW := w / 4
	lc2Sample := func(px, py int) {
		if px >= w || py >= h {
			return
		}
		off := (py*w + px) * 4
		if off+3 >= len(combined) {
			return
		}
		uvRow := py >> 1
		var actualCb, actualCr byte
		var barea string
		if px&1 == 1 {
			// B4/B5: odd column — Cb/Cr packed in stream2 Y plane.
			barea = "B4/B5"
			k := px >> 1
			auxYRow := i420aux.Y[py*i420aux.YStride:]
			actualCb = auxYRow[k]
			actualCr = auxYRow[halfW+k]
		} else if py&1 == 0 {
			// B2/B3: even column, even row — from stream1 cached chroma.
			barea = "B2/B3"
			actualCb = yp.u[uvRow*yp.uvStride+(px>>1)]
			actualCr = yp.v[uvRow*yp.uvStride+(px>>1)]
		} else {
			k2 := px >> 2
			if px&2 == 0 {
				// B6/B7: even column (col%4==0), odd row.
				barea = "B6/B7"
				actualCb = i420aux.U[uvRow*i420aux.UStride+k2]
				actualCr = i420aux.U[uvRow*i420aux.UStride+quarterW+k2]
			} else {
				// B8/B9: even column (col%4==2), odd row.
				barea = "B8/B9"
				actualCb = i420aux.V[uvRow*i420aux.VStride+k2]
				actualCr = i420aux.V[uvRow*i420aux.VStride+quarterW+k2]
			}
		}
		slog.Debug("H.264: pixel sample (LC=2 combine)",
			"x", px, "y", py,
			"area", barea,
			"isIDR", stream2IsIDR,
			"usedIDRSnapshot", yp == &g.avc444IDRYPlane,
			"Y1", yp.data[py*yp.stride+px],
			"Cb", actualCb, "Cr", actualCr,
			"B", combined[off], "G", combined[off+1], "R", combined[off+2])
	}
	if !g.lc2SampleLogged {
		g.lc2SampleLogged = true
		// B2/B3 (even col, even row)
		lc2Sample(100, 50)
		lc2Sample(500, 50)
		// B4/B5 (odd col) — most important for diagnosing tint artifacts
		lc2Sample(101, 50)
		lc2Sample(501, 50)
		lc2Sample(961, 50)
		// B6/B7 (col%4==0, odd row)
		lc2Sample(100, 51)
		lc2Sample(500, 51)
		// B8/B9 (col%4==2, odd row)
		lc2Sample(102, 51)
		lc2Sample(502, 51)
		// video area — all four B-areas near the same spot
		lc2Sample(960, 600)
		lc2Sample(961, 600)
		lc2Sample(960, 601)
		lc2Sample(962, 601)
	} else if !g.lc2PFrameSampleLogged && !stream2IsIDR {
		g.lc2PFrameSampleLogged = true
		lc2Sample(100, 50)
		lc2Sample(101, 50)
		lc2Sample(100, 51)
		lc2Sample(102, 51)
		lc2Sample(500, 50)
		lc2Sample(501, 50)
		lc2Sample(960, 400)
		lc2Sample(961, 400)
		lc2Sample(960, 401)
		lc2Sample(962, 401)
		lc2Sample(960, 600)
		lc2Sample(961, 600)
	}
	decoded, pooled = cropBGRA(combined, w, h, destW, destH)
	if w == destW && h == destH {
		// cropBGRA returned combined unchanged; mark as pooled so caller releases it.
		pooled = true
	} else {
		// cropBGRA created a new buffer; release the intermediate combined buffer.
		releaseBitmapBuf(combined)
	}
	regions = stream2.regions
	slog.Debug("RDPGFX: AVC444 LC=2 decoded", "w", w, "h", h,
		"destW", destW, "destH", destH, "h264Len", len(stream2.h264Data))
	g.noteSuccessfulDecode()
	return
}

// softResetLimit is the number of in-place decoder recreations attempted
// before escalating to a full RDP reconnect.
const softResetLimit = 5

// maybeRequestKeyframe sends a keyframe request to the server when either
// decoder needs a fresh IDR.  Requests are rate-limited to once per 2 seconds
// so that repeated nil-frame callbacks (e.g. while waiting for the IDR) don't
// flood the server.  This covers both post-flush and post-soft-reset cases,
// including the case where h264dec2 was reset independently of h264dec.
func (g *GfxHandler) maybeRequestKeyframe() {
	if g.h264dec == nil || g.h264dec.IsBroken() {
		return
	}
	dec1NeedsKF := g.h264dec.NeedsKeyframe()
	// Do NOT include h264dec2 here: ForceRefresh only triggers an LC=1 luma IDR
	// from the server.  The stream2/chroma IDR is never delivered via
	// ForceRefresh — it arrives naturally as an LC=0 frame via primeAuxDecoder.
	// Requesting ForceRefresh because h264dec2.NeedsIDR()=true spams the server
	// with keyframe requests, causes the server to repeatedly send LC=1 IDRs,
	// and can deadlock the main VideoToolbox decoder.
	if !dec1NeedsKF {
		return
	}
	const keyframeRequestInterval = 2 * time.Second
	if time.Since(g.lastKeyframeRequest) < keyframeRequestInterval {
		return
	}
	g.lastKeyframeRequest = time.Now()
	if g.onKeyframeRequest != nil {
		go g.onKeyframeRequest()
	}
}

// maybeNotifyDecoderBroken is called whenever the H.264 decoder returns a
// nil frame.  It first tries up to softResetLimit in-place decoder resets
// (cheap: just recreate the FFmpeg/VideoToolbox context and ask the server
// for a fresh IDR).  Only after all soft resets are exhausted does it call
// onDecoderBroken, which triggers a full RDP reconnect.
func (g *GfxHandler) maybeNotifyDecoderBroken() {
	if g.decoderBrokenNotified {
		return
	}
	if g.h264dec == nil || !g.h264dec.IsBroken() {
		return
	}
	reason := g.h264dec.BrokenReason()
	if reason == h264BrokenReasonNoIDR {
		// Allow only one no-IDR soft reset before escalating to reconnect.
		// ForceRefresh (SuppressOutput toggle) often fails to trigger a new
		// AVC444 IDR from Windows servers; repeatedly retrying just prolongs
		// the freeze.  One attempt gives the server a fair chance; after that
		// a full reconnect is faster than waiting another 10+ seconds per try.
		const softResetLimitNoIDR = 1
		if g.softResetCount < softResetLimitNoIDR {
			g.softResetCount++
			slog.Debug("H.264: soft decoder reset (no-IDR)",
				"attempt", g.softResetCount, "limit", softResetLimitNoIDR,
				"reason", reason.String())
			g.h264dec.Close()
			if g.usingSWFallback {
				g.h264dec = newH264DecoderSWWithWatchdog(g.watchdogCh)
			} else {
				g.h264dec = newH264DecoderWithWatchdog(g.watchdogCh)
			}
			if g.h264dec2 != nil && g.h264dec2.IsBroken() {
				slog.Debug("H.264: aux decoder also broken on soft reset, waiting for IDR to recreate")
				g.h264dec2.Close()
				g.h264dec2 = nil
			}
			g.lastKeyframeRequest = time.Time{}
			g.maybeRequestKeyframe()
			return
		}
		slog.Debug("H.264: escalating to reconnect after no-IDR soft reset exhausted",
			"reason", reason.String())
		g.decoderBrokenNotified = true
		if g.onDecoderBroken != nil {
			go g.onDecoderBroken()
		}
		return
	}
	if g.softResetCount < softResetLimit {
		g.softResetCount++
		if reason == h264BrokenReasonHWStall && !g.usingSWFallback {
			slog.Debug("H.264: HW stall — falling back to software decoding",
				"attempt", g.softResetCount, "limit", softResetLimit)
			g.usingSWFallback = true
		} else {
			slog.Debug("H.264: soft decoder reset",
				"attempt", g.softResetCount, "limit", softResetLimit,
				"reason", reason.String())
		}
		g.h264dec.Close()
		if g.usingSWFallback {
			g.h264dec = newH264DecoderSWWithWatchdog(g.watchdogCh)
		} else {
			g.h264dec = newH264DecoderWithWatchdog(g.watchdogCh)
		}
		// Keep h264dec2 if healthy; tear it down if already broken so
		// primeAuxDecoder can recreate it when the next stream2 IDR arrives,
		// rather than spinning up a new VT session only to have it break again.
		// Always keep avc444YPlane so that LC=2 frames can continue to display
		// stale-but-reasonable content during recovery.
		if g.h264dec2 != nil && g.h264dec2.IsBroken() {
			slog.Debug("H.264: aux decoder also broken on soft reset, waiting for IDR to recreate")
			g.h264dec2.Close()
			g.h264dec2 = nil
		}
		// Reset rate-limiter so keyframe request fires immediately after reset.
		g.lastKeyframeRequest = time.Time{}
		g.maybeRequestKeyframe()
		return
	}
	// All soft resets exhausted — escalate to full reconnect.
	g.decoderBrokenNotified = true
	if g.onDecoderBroken != nil {
		go g.onDecoderBroken()
	}
}

// cropBGRA crops or pads BGRA pixel data to the target dimensions.
// When srcW == dstW and srcH == dstH the input slice is returned unchanged
// and pooled is false.  Otherwise a new buffer is acquired from bitmapBufPool
// (pooled == true) and the caller must call releaseBitmapBuf on it.
func cropBGRA(src []byte, srcW, srcH, dstW, dstH int) ([]byte, bool) {
	if srcW == dstW && srcH == dstH {
		return src, false
	}
	out := acquireBitmapBuf(dstW * dstH * 4)
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
	return out, true
}

// avcRegionUseThresholdPercent is the upper bound on the *fraction* of the
// decoded frame area that the union of dirty rects can cover before we give
// up and just blit the whole frame.  When the dirty area approaches the
// total area, the per-rect bookkeeping (allocation per rect, separate
// BitmapUpdate per rect) costs more than the bytes-copied savings.
const avcRegionUseThresholdPercent = 60

// shouldUseAVCRegions returns true when the per-region partial blit path is
// expected to be cheaper than a single full-frame blit.  A single region
// covering everything is treated as "no win"; many tiny regions covering
// most of the frame are similarly bypassed.
func shouldUseAVCRegions(regions []avcRect, frameW, frameH int) bool {
	if frameW <= 0 || frameH <= 0 {
		return false
	}
	total := frameW * frameH
	if total == 0 {
		return false
	}
	// Sum (with overlap double-counting) — overlap is uncommon in practice
	// and the threshold leaves slack for it.
	sum := 0
	for _, r := range regions {
		if r.right <= r.left || r.bottom <= r.top {
			continue
		}
		w := int(r.right - r.left)
		h := int(r.bottom - r.top)
		sum += w * h
		if sum*100 >= total*avcRegionUseThresholdPercent {
			return false
		}
	}
	return sum > 0
}

// blitAndEmitAVCRegions copies only the dirty rectangles of a decoded AVC
// frame into the persistent surface and emits a BitmapUpdate per region.
// All region coordinates are in decoded-frame space (i.e. relative to
// (left, top) on the surface).
//
// The emitted Data buffers are borrowed from bitmapBufPool and are returned
// to the pool once the synchronous onBitmap callback completes — see the
// BitmapUpdate lifecycle note.
func (g *GfxHandler) blitAndEmitAVCRegions(s *surface, left, top, frameW, frameH int, decoded []byte, regions []avcRect) {
	frameStride := frameW * 4
	surfStride := int(s.width) * 4
	updates := make([]BitmapUpdate, 0, len(regions))
	for _, rc := range regions {
		if rc.right <= rc.left || rc.bottom <= rc.top {
			continue
		}
		rx, ry := int(rc.left), int(rc.top)
		rw, rh := int(rc.right-rc.left), int(rc.bottom-rc.top)
		if rx+rw > frameW {
			rw = frameW - rx
		}
		if ry+rh > frameH {
			rh = frameH - ry
		}
		if rw <= 0 || rh <= 0 {
			continue
		}
		rowBytes := rw * 4
		region := acquireBitmapBuf(rw * rh * 4)
		for row := 0; row < rh; row++ {
			srcOff := (ry+row)*frameStride + rx*4
			if srcOff+rowBytes > len(decoded) {
				break
			}
			copy(region[row*rowBytes:row*rowBytes+rowBytes],
				decoded[srcOff:srcOff+rowBytes])

			// Mirror the same row into the persistent surface so any
			// subsequent codec (RFX progressive etc.) operating on the
			// same surface starts from the up-to-date pixels.
			dy := top + ry + row
			if dy < 0 || dy >= int(s.height) {
				continue
			}
			dstOff := dy*surfStride + (left+rx)*4
			if dstOff < 0 || dstOff+rowBytes > len(s.data) {
				continue
			}
			copy(s.data[dstOff:dstOff+rowBytes],
				decoded[srcOff:srcOff+rowBytes])
		}
		if !s.mapped || g.onBitmap == nil {
			releaseBitmapBuf(region)
			continue
		}
		destL := int(s.outputX) + left + rx
		destT := int(s.outputY) + top + ry
		updates = append(updates, BitmapUpdate{
			DestLeft: destL, DestTop: destT,
			DestRight: destL + rw - 1, DestBottom: destT + rh - 1,
			Width: rw, Height: rh, Bpp: 4, Data: region,
		})
	}
	g.emitAndReleaseUpdates(updates)
}
