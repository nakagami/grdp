//go:build h264

package rdpgfx

/*
#cgo pkg-config: libavcodec libavutil libswscale
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/hwcontext.h>
#include <libavutil/log.h>
#include <libswscale/swscale.h>
#include <stdlib.h>
#include <stdint.h>

// grdp_suppress_av_log sets FFmpeg's global log level to FATAL so that
// decoder-level error messages (e.g. "sps_id out of range", "no frame!")
// are not printed to stderr.  Those messages are expected and harmless
// during H.264 stream recovery; grdp emits its own slog warnings instead.
static void grdp_suppress_av_log(void) {
    av_log_set_level(AV_LOG_FATAL);
}

// get_format callback that prefers the hardware pixel format stored in opaque.
static enum AVPixelFormat grdp_get_hw_format(
    AVCodecContext *ctx, const enum AVPixelFormat *pix_fmts) {
    enum AVPixelFormat hw_fmt = (enum AVPixelFormat)(intptr_t)ctx->opaque;
    if (hw_fmt == AV_PIX_FMT_NONE) return pix_fmts[0];
    for (const enum AVPixelFormat *p = pix_fmts; *p != AV_PIX_FMT_NONE; p++) {
        if (*p == hw_fmt) return *p;
    }
    return pix_fmts[0];
}

static void grdp_set_get_format(AVCodecContext *ctx) {
    ctx->get_format = grdp_get_hw_format;
}

static void grdp_set_hw_pix_fmt(AVCodecContext *ctx, enum AVPixelFormat fmt) {
    ctx->opaque = (void*)(intptr_t)fmt;
}

// Helper: convert AVFrame to BGRA via swscale.
static int grdp_frame_to_bgra(struct SwsContext *sws,
    AVFrame *src, uint8_t *dst, int dst_stride) {
    uint8_t *dst_data[4] = {dst, NULL, NULL, NULL};
    int dst_linesize[4] = {dst_stride, 0, 0, 0};
    return sws_scale(sws,
        (const uint8_t *const *)src->data, src->linesize,
        0, src->height,
        dst_data, dst_linesize);
}

// Map deprecated YUVJ pixel formats to their non-J equivalents.
// YUVJ formats are full-range YUV; the modern way is to use the plain YUV
// format and communicate the range via sws_setColorspaceDetails.
static enum AVPixelFormat grdp_yuvj_to_yuv(enum AVPixelFormat fmt) {
    switch (fmt) {
    case AV_PIX_FMT_YUVJ420P: return AV_PIX_FMT_YUV420P;
    case AV_PIX_FMT_YUVJ422P: return AV_PIX_FMT_YUV422P;
    case AV_PIX_FMT_YUVJ444P: return AV_PIX_FMT_YUV444P;
    case AV_PIX_FMT_YUVJ440P: return AV_PIX_FMT_YUV440P;
    default: return fmt;
    }
}

// Return 1 if fmt is a full-range (YUVJ) format, 0 otherwise.
static int grdp_is_full_range_fmt(enum AVPixelFormat fmt) {
    return (fmt == AV_PIX_FMT_YUVJ420P ||
            fmt == AV_PIX_FMT_YUVJ422P ||
            fmt == AV_PIX_FMT_YUVJ444P ||
            fmt == AV_PIX_FMT_YUVJ440P) ? 1 : 0;
}

// grdp_yuv420p_to_bgra converts a planar YUV420P/YUVJ420P frame to packed
// BGRA in-place using BT.601 coefficients.  This bypasses swscale entirely
// so that the broken ARM64 colorspace-matrix fallback path is never taken.
// full_range: 0 = limited (video) range [16-235 / 16-240],
//             1 = full range [0-255].
static void grdp_yuv420p_to_bgra(
    const AVFrame *src, uint8_t *dst, int dst_stride, int full_range)
{
    int width  = src->width;
    int height = src->height;
    for (int row = 0; row < height; row++) {
        const uint8_t *yrow = src->data[0] + row           * src->linesize[0];
        const uint8_t *urow = src->data[1] + (row >> 1)    * src->linesize[1];
        const uint8_t *vrow = src->data[2] + (row >> 1)    * src->linesize[2];
        uint8_t *drow = dst + row * dst_stride;
        for (int col = 0; col < width; col++) {
            int u = (int)urow[col >> 1] - 128;
            int v = (int)vrow[col >> 1] - 128;
            int r, g, b;
            if (full_range) {
                int y = (int)yrow[col];
                r = (256*y + 359*v           + 128) >> 8;
                g = (256*y -  88*u - 183*v   + 128) >> 8;
                b = (256*y + 454*u           + 128) >> 8;
            } else {
                int c = (int)yrow[col] - 16;
                r = (298*c + 409*v           + 128) >> 8;
                g = (298*c - 100*u - 208*v   + 128) >> 8;
                b = (298*c + 516*u           + 128) >> 8;
            }
#define CLAMP8(x) ((x) < 0 ? 0 : (x) > 255 ? 255 : (uint8_t)(x))
            drow[col*4    ] = CLAMP8(b);
            drow[col*4 + 1] = CLAMP8(g);
            drow[col*4 + 2] = CLAMP8(r);
            drow[col*4 + 3] = 255;
#undef CLAMP8
        }
    }
}

// grdp_sample_yuv samples the centre pixel of a planar YUV frame for
// diagnostics.  Returns raw byte values (not offset-adjusted).
static void grdp_sample_yuv(const AVFrame *f,
    uint8_t *y_out, uint8_t *u_out, uint8_t *v_out)
{
    int cx = f->width  / 2;
    int cy = f->height / 2;
    *y_out = f->data[0][ cy      * f->linesize[0] +  cx     ];
    *u_out = f->data[1][(cy / 2) * f->linesize[1] + (cx / 2)];
    *v_out = f->data[2][(cy / 2) * f->linesize[2] + (cx / 2)];
}

static void grdp_sws_set_src_range(struct SwsContext *sws, int full_range) {
    const int *inv_table, *table;
    int src_range, dst_range, brightness, contrast, saturation;
    if (sws_getColorspaceDetails(sws,
            (int **)&inv_table, &src_range,
            (int **)&table,     &dst_range,
            &brightness, &contrast, &saturation) >= 0) {
        sws_setColorspaceDetails(sws,
            inv_table, full_range,
            table,     dst_range,
            brightness, contrast, saturation);
    }
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"unsafe"
)

// avLogOnce ensures grdp_suppress_av_log is called only once per process.
var avLogOnce sync.Once

// hwStallThreshold is the number of consecutive nil-frame results *after the
// hardware decoder has already produced at least one frame* that triggers a
// recovery attempt.  Counted only once HW has proven it works, so normal VT
// initialisation delay (a few frames of EAGAIN) does not count.
// VideoToolbox may delay output by several frames due to B-frame reordering,
// so this must be generous enough to avoid false positives on normal streams,
// but not so large that stalls (e.g. from YouTube adaptive bitrate switches)
// cause long video freezes before recovery is attempted.
const hwStallThreshold = 10

// hwInitTimeout is the maximum number of packets sent to the hardware decoder
// before it has produced its first frame.  If VT never produces output within
// this many input packets it is considered permanently broken and we switch to
// software.  This covers streams that VT cannot decode at all (e.g. profiles
// it does not support).
const hwInitTimeout = 60

// swStallThreshold is the equivalent threshold for the software decoder.
const swStallThreshold = 30

// hwMaxRecoveries is the number of times the HW decoder is given a chance to
// recover (via avcodec_flush_buffers + IDR resync) before permanently falling
// back to software.  Each attempt flushes the VT pipeline and waits for a
// fresh IDR from the server, so 3 attempts gives the server ~9 seconds to
// deliver a keyframe before we give up on hardware acceleration.
const hwMaxRecoveries = 3

// keyframeWaitLimit is the maximum number of non-IDR packets we drop while
// waiting for a keyframe after a decoder reset.  If the server never sends a
// new IDR within this many packets we give up waiting and feed P-frames to the
// SW decoder anyway; FFmpeg's error-concealment keeps the session alive.
const keyframeWaitLimit = 150

type ffmpegDecoder struct {
	codecCtx  *C.AVCodecContext
	packet    *C.AVPacket
	frame     *C.AVFrame
	swFrame   *C.AVFrame
	swsCtx    *C.struct_SwsContext
	useHW     bool
	hwPixFmt  C.enum_AVPixelFormat
	lastW     C.int
	lastH     C.int
	lastFmt   C.enum_AVPixelFormat
	stallCount        int  // consecutive Decode() calls that returned nil
	needsKeyFrame     bool // drop packets until an IDR/SPS is received
	keyframeWaitCount int  // P-frames dropped so far while needsKeyFrame=true
	hwReady           bool // HW decoder has produced at least one frame
	hwSentCount       int  // packets sent to HW decoder (for init timeout)
	hwErrorCount      int  // consecutive avcodec_send_packet hard errors on HW
	hwRecoveries      int  // number of HW stall recovery attempts made
	swFrameCount      int  // frames decoded by SW decoder (for diagnostics)
}

func newH264Decoder() h264Decoder {
	// Suppress FFmpeg stderr output (e.g. "[h264 @ ...] sps_id out of range").
	// grdp emits its own slog messages for H.264 recovery events.
	avLogOnce.Do(func() { C.grdp_suppress_av_log() })

	codec := C.avcodec_find_decoder(C.AV_CODEC_ID_H264)
	if codec == nil {
		slog.Warn("H.264: codec not found in FFmpeg")
		return nil
	}

	codecCtx := C.avcodec_alloc_context3(codec)
	if codecCtx == nil {
		return nil
	}

	d := &ffmpegDecoder{
		codecCtx: codecCtx,
		hwPixFmt: C.AV_PIX_FMT_NONE,
		lastFmt:  C.AV_PIX_FMT_NONE,
	}

	// Probe available hardware acceleration backends.
	hwType := C.av_hwdevice_iterate_types(C.AV_HWDEVICE_TYPE_NONE)
	for hwType != C.AV_HWDEVICE_TYPE_NONE {
		var devCtx *C.AVBufferRef
		if C.av_hwdevice_ctx_create(&devCtx, hwType, nil, nil, 0) == 0 {
			// Find the HW pixel format for this device type.
			hwPixFmt := C.enum_AVPixelFormat(C.AV_PIX_FMT_NONE)
			for i := C.int(0); ; i++ {
				cfg := C.avcodec_get_hw_config(codec, i)
				if cfg == nil {
					break
				}
				if cfg.device_type == hwType &&
					(cfg.methods&C.AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX) != 0 {
					hwPixFmt = cfg.pix_fmt
					break
				}
			}

			if hwPixFmt != C.AV_PIX_FMT_NONE {
				codecCtx.hw_device_ctx = C.av_buffer_ref(devCtx)
				C.grdp_set_hw_pix_fmt(codecCtx, hwPixFmt)
				C.grdp_set_get_format(codecCtx)
				d.useHW = true
				d.hwPixFmt = hwPixFmt
				name := C.av_hwdevice_get_type_name(hwType)
				slog.Debug("H.264: hardware acceleration enabled", "type", C.GoString(name))
			}
			C.av_buffer_unref(&devCtx)
			if d.useHW {
				break
			}
		}
		hwType = C.av_hwdevice_iterate_types(hwType)
	}

	if !d.useHW {
		slog.Debug("H.264: using software decoding")
	}

	if C.avcodec_open2(codecCtx, codec, nil) < 0 {
		C.avcodec_free_context(&d.codecCtx)
		return nil
	}

	d.packet = C.av_packet_alloc()
	d.frame = C.av_frame_alloc()
	d.swFrame = C.av_frame_alloc()
	if d.packet == nil || d.frame == nil || d.swFrame == nil {
		d.Close()
		return nil
	}

	runtime.SetFinalizer(d, func(dec *ffmpegDecoder) { dec.Close() })
	return d
}

func (d *ffmpegDecoder) NeedsKeyframe() bool {
	return d.needsKeyFrame
}

func (d *ffmpegDecoder) Decode(h264Data []byte) (*h264Frame, error) {
	if len(h264Data) == 0 {
		return nil, nil
	}

	// After a decoder reset we must resync with a fresh IDR from the server.
	// Priming with a cached IDR does NOT work: P-frames mid-GOP expect a DPB
	// containing several preceding reference frames that we no longer have.
	// Feeding only the cached IDR causes the SW decoder to output zero-filled
	// frames (all Y=U=V=0) which render as solid green.  The correct recovery
	// is to wait for the server to begin a new GOP.  maybeRequestKeyframe()
	// calls SendRefreshRect every 3 s to prompt the server.
	// If the server never sends an IDR within keyframeWaitLimit packets we
	// fall back to SW error-concealment so the session does not hang.
	// FFmpeg's "[h264 @ ...] sps_id out of range" errors are suppressed at
	// the av_log level (AV_LOG_FATAL) set in newH264Decoder; grdp emits its
	// own slog warning instead.
	if d.needsKeyFrame {
		if !h264ContainsKeyFrame(h264Data) {
			d.keyframeWaitCount++
			if d.keyframeWaitCount >= keyframeWaitLimit {
				slog.Warn("H.264: no IDR received, proceeding without keyframe",
					"waited", d.keyframeWaitCount)
				d.needsKeyFrame = false
				d.keyframeWaitCount = 0
				// fall through and attempt SW error-concealment decode
			} else {
				return nil, nil // drop P-frames while waiting
			}
		} else {
			d.needsKeyFrame = false
			d.keyframeWaitCount = 0
		}
	}

	// If the decoder has been stalled for too long (e.g. VideoToolbox
	// rejecting frames due to reference-frame count limits), try to recover.
	threshold := swStallThreshold
	if d.useHW {
		threshold = hwStallThreshold
	}
	if d.stallCount >= threshold {
		if d.useHW {
			// VideoToolbox is returning null frames — it may be stalled due to
			// a stream parameter change (e.g. YouTube adaptive bitrate switch).
			// Flush the VT pipeline and request a fresh IDR from the server so
			// it begins a new GOP.  This is far more effective than simply
			// resetting the stall counter, which just waits and hopes.
			if d.hwRecoveries < hwMaxRecoveries {
				slog.Debug("H.264: HW decoder stalled, flushing and requesting keyframe",
					"stallCount", d.stallCount,
					"attempt", d.hwRecoveries+1,
					"maxAttempts", hwMaxRecoveries)
				C.avcodec_flush_buffers(d.codecCtx)
				d.needsKeyFrame = true
				d.keyframeWaitCount = 0
				d.stallCount = 0
				d.hwRecoveries++
				return nil, nil
			}
			slog.Warn("H.264: HW decoder stalled, switching to software decoding",
				"stallCount", d.stallCount,
				"recoveryAttempts", d.hwRecoveries)
			d.reinitSoftware()
			return nil, nil
		}
		slog.Debug("H.264: SW decoder stalled, flushing", "stallCount", d.stallCount)
		C.avcodec_flush_buffers(d.codecCtx)
		d.stallCount = 0
	}

	cData := C.CBytes(h264Data)
	defer C.free(cData)

	d.packet.data = (*C.uint8_t)(cData)
	d.packet.size = C.int(len(h264Data))

	// Count packets sent to HW decoder (for init timeout tracking).
	if d.useHW {
		d.hwSentCount++
	}

	ret := C.avcodec_send_packet(d.codecCtx, d.packet)
	if ret < 0 {
		if d.useHW {
			// VideoToolbox hard error (e.g. AVERROR_UNKNOWN): flush_buffers
			// does not recover this state.  After a few consecutive failures
			// fall back to software so the session stays alive.
			d.hwErrorCount++
			slog.Debug("H.264: HW avcodec_send_packet failed",
				"err", int(ret), "hwErrorCount", d.hwErrorCount)
			if d.hwErrorCount >= 5 {
				slog.Warn("H.264: HW decoder hard error, switching to software",
					"hwErrorCount", d.hwErrorCount)
				d.reinitSoftware()
			}
			return nil, nil
		}
		// SW decoder: flush and wait for a new IDR.
		slog.Debug("H.264: avcodec_send_packet failed, flushing decoder to recover",
			"err", int(ret))
		C.avcodec_flush_buffers(d.codecCtx)
		d.stallCount = 0
		d.needsKeyFrame = true
		d.keyframeWaitCount = 0
		return nil, nil
	}
	d.hwErrorCount = 0

	// Receive decoded frame(s); keep the last one.
	var result *h264Frame
	for {
		ret = C.avcodec_receive_frame(d.codecCtx, d.frame)
		if ret < 0 {
			break // EAGAIN (need more input) or EOF
		}
		f, err := d.convertFrame()
		C.av_frame_unref(d.frame)
		if err != nil {
			return nil, err
		}
		result = f
	}

	if result != nil {
		d.stallCount = 0
		if d.useHW {
			if !d.hwReady {
				slog.Debug("H.264: HW decoder produced first frame",
					"hwSentCount", d.hwSentCount)
			}
			d.hwReady = true // HW has proven it can produce frames
			d.hwRecoveries = 0 // successful decode, reset recovery counter
		}
	} else {
		// Only count stalls once HW has produced at least one frame.
		// Before that, EAGAIN is normal initialisation behaviour.
		// However, if HW never produces a frame within hwInitTimeout packets,
		// treat that as a permanent failure too.
		if !d.useHW || d.hwReady {
			d.stallCount++
			if d.useHW {
				slog.Debug("H.264: HW stall tick", "stallCount", d.stallCount,
					"threshold", hwStallThreshold, "hwSentCount", d.hwSentCount)
			}
		} else if d.hwSentCount >= hwInitTimeout {
			slog.Warn("H.264: HW decoder failed to produce first frame, switching to software",
				"hwSentCount", d.hwSentCount)
			d.reinitSoftware()
			return nil, nil
		}
	}
	return result, nil
}

func (d *ffmpegDecoder) convertFrame() (*h264Frame, error) {
	srcFrame := d.frame

	// Transfer from GPU to CPU memory if using hardware acceleration.
	if d.useHW && d.frame.format == C.int(d.hwPixFmt) {
		ret := C.av_hwframe_transfer_data(d.swFrame, d.frame, 0)
		if ret < 0 {
			return nil, fmt.Errorf("av_hwframe_transfer_data: error %d", int(ret))
		}
		srcFrame = d.swFrame
	}

	w := srcFrame.width
	h := srcFrame.height
	srcFmt := C.enum_AVPixelFormat(srcFrame.format)

	outSize := int(w) * int(h) * 4
	out := make([]byte, outSize)

	// For planar YUV420P (both limited- and full-range variants), use our own
	// BT.601 conversion instead of swscale.  On ARM64, swscale has no
	// accelerated colorspace-conversion path for yuv420p→bgra and its
	// non-accelerated fallback ignores sws_setColorspaceDetails, producing
	// a strong green cast.  The hand-written C loop is guaranteed correct.
	if srcFmt == C.AV_PIX_FMT_YUV420P || srcFmt == C.AV_PIX_FMT_YUVJ420P {
		fullRange := C.int(0)
		if srcFmt == C.AV_PIX_FMT_YUVJ420P || srcFrame.color_range == 2 {
			fullRange = 1
		}
		// Log the centre-pixel YUV values for the first few SW frames so we
		// can distinguish H.264 decode corruption from colour-conversion bugs.
		if !d.useHW && d.swFrameCount < 3 {
			var sy, su, sv C.uint8_t
			C.grdp_sample_yuv(srcFrame, &sy, &su, &sv)
			slog.Info("H.264: SW frame sample",
				"frame", d.swFrameCount,
				"fmt", int(srcFmt),
				"fullRange", int(fullRange),
				"Y", int(sy), "U", int(su), "V", int(sv),
				"w", int(w), "h", int(h))
			d.swFrameCount++
		}
		C.grdp_yuv420p_to_bgra(srcFrame,
			(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4), fullRange)
		if srcFrame == d.swFrame {
			C.av_frame_unref(d.swFrame)
		}
		return &h264Frame{Data: out, Width: int(w), Height: int(h)}, nil
	}

	// For other formats (e.g. NV12 from VideoToolbox HW transfer), use swscale.
	swsFmt := C.grdp_yuvj_to_yuv(srcFmt)
	fullRange := C.grdp_is_full_range_fmt(srcFmt)
	if fullRange == 0 && srcFrame.color_range == 2 { // AVCOL_RANGE_JPEG
		fullRange = 1
	}

	if w != d.lastW || h != d.lastH || srcFmt != d.lastFmt {
		if d.swsCtx != nil {
			C.sws_freeContext(d.swsCtx)
		}
		d.swsCtx = C.sws_getContext(
			w, h, swsFmt,
			w, h, C.AV_PIX_FMT_BGRA,
			C.SWS_BILINEAR, nil, nil, nil,
		)
		if d.swsCtx == nil {
			return nil, fmt.Errorf("sws_getContext failed for %dx%d fmt=%d", w, h, srcFmt)
		}
		C.grdp_sws_set_src_range(d.swsCtx, fullRange)
		d.lastW = w
		d.lastH = h
		d.lastFmt = srcFmt
	}

	C.grdp_frame_to_bgra(d.swsCtx, srcFrame,
		(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4))

	if srcFrame == d.swFrame {
		C.av_frame_unref(d.swFrame)
	}

	return &h264Frame{
		Data:   out,
		Width:  int(w),
		Height: int(h),
	}, nil
}

// h264ContainsKeyFrame scans an Annex B H.264 bitstream and returns true if
// it contains a SPS (type 7) or IDR slice (type 5) NAL unit.  Such a packet
// carries all the parameter sets needed by a freshly-initialised decoder.
func h264ContainsKeyFrame(data []byte) bool {
	for i := 0; i < len(data)-3; i++ {
		// Look for 3-byte or 4-byte Annex B start code.
		var naluStart int
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			naluStart = i + 3
		} else if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 &&
			data[i+2] == 0 && data[i+3] == 1 {
			naluStart = i + 4
		} else {
			continue
		}
		if naluStart >= len(data) {
			break
		}
		naluType := data[naluStart] & 0x1F
		if naluType == 5 || naluType == 7 { // IDR or SPS
			return true
		}
	}
	return false
}

// reinitSoftware tears down the HW-accelerated codec context and reopens
// the H.264 decoder in pure software mode.  Called when the HW decoder
// (e.g. VideoToolbox) is permanently broken for the current stream.
func (d *ffmpegDecoder) reinitSoftware() {
	if d.swsCtx != nil {
		C.sws_freeContext(d.swsCtx)
		d.swsCtx = nil
	}
	if d.codecCtx != nil {
		C.avcodec_free_context(&d.codecCtx)
	}

	codec := C.avcodec_find_decoder(C.AV_CODEC_ID_H264)
	if codec == nil {
		return
	}
	d.codecCtx = C.avcodec_alloc_context3(codec)
	if d.codecCtx == nil {
		return
	}
	if C.avcodec_open2(d.codecCtx, codec, nil) < 0 {
		C.avcodec_free_context(&d.codecCtx)
		return
	}

	d.useHW = false
	d.hwPixFmt = C.AV_PIX_FMT_NONE
	d.lastW = 0
	d.lastH = 0
	d.lastFmt = C.AV_PIX_FMT_NONE
	d.stallCount = 0
	d.needsKeyFrame = true
	d.keyframeWaitCount = 0
	d.hwReady = false
	d.hwSentCount = 0
	d.hwRecoveries = 0
	d.swFrameCount = 0
}

func (d *ffmpegDecoder) Close() {
	if d.swsCtx != nil {
		C.sws_freeContext(d.swsCtx)
		d.swsCtx = nil
	}
	if d.frame != nil {
		C.av_frame_free(&d.frame)
	}
	if d.swFrame != nil {
		C.av_frame_free(&d.swFrame)
	}
	if d.packet != nil {
		C.av_packet_free(&d.packet)
	}
	if d.codecCtx != nil {
		C.avcodec_free_context(&d.codecCtx)
	}
}
