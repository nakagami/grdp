//go:build h264

package rdpgfx

/*
#cgo pkg-config: libavcodec libavutil libswscale
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/hwcontext.h>
#include <libswscale/swscale.h>
#include <stdlib.h>
#include <stdint.h>

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
*/
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
	"unsafe"
)

type ffmpegDecoder struct {
	codecCtx *C.AVCodecContext
	packet   *C.AVPacket
	frame    *C.AVFrame
	swFrame  *C.AVFrame
	swsCtx   *C.struct_SwsContext
	useHW    bool
	hwPixFmt C.enum_AVPixelFormat
	lastW    C.int
	lastH    C.int
	lastFmt  C.enum_AVPixelFormat
}

func newH264Decoder() h264Decoder {
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
				codecCtx.opaque = unsafe.Pointer(uintptr(hwPixFmt))
				C.grdp_set_get_format(codecCtx)
				d.useHW = true
				d.hwPixFmt = hwPixFmt
				name := C.av_hwdevice_get_type_name(hwType)
				slog.Info("H.264: hardware acceleration enabled", "type", C.GoString(name))
			}
			C.av_buffer_unref(&devCtx)
			if d.useHW {
				break
			}
		}
		hwType = C.av_hwdevice_iterate_types(hwType)
	}

	if !d.useHW {
		slog.Info("H.264: using software decoding")
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

func (d *ffmpegDecoder) Decode(h264Data []byte) (*h264Frame, error) {
	if len(h264Data) == 0 {
		return nil, nil
	}

	cData := C.CBytes(h264Data)
	defer C.free(cData)

	d.packet.data = (*C.uint8_t)(cData)
	d.packet.size = C.int(len(h264Data))

	ret := C.avcodec_send_packet(d.codecCtx, d.packet)
	if ret < 0 {
		return nil, fmt.Errorf("avcodec_send_packet: error %d", int(ret))
	}

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

	// Recreate swscale context when dimensions or format change.
	if w != d.lastW || h != d.lastH || srcFmt != d.lastFmt {
		if d.swsCtx != nil {
			C.sws_freeContext(d.swsCtx)
		}
		d.swsCtx = C.sws_getContext(
			w, h, srcFmt,
			w, h, C.AV_PIX_FMT_BGRA,
			C.SWS_BILINEAR, nil, nil, nil,
		)
		if d.swsCtx == nil {
			return nil, fmt.Errorf("sws_getContext failed for %dx%d fmt=%d", w, h, srcFmt)
		}
		d.lastW = w
		d.lastH = h
		d.lastFmt = srcFmt
	}

	outSize := int(w) * int(h) * 4
	out := make([]byte, outSize)
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
