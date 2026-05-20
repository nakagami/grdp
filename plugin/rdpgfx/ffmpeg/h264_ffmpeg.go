//go:build h264

package ffmpeg

/*
#cgo pkg-config: libavcodec libavutil libswscale
#cgo nocallback avcodec_alloc_context3
#cgo nocallback avcodec_find_decoder
#cgo nocallback avcodec_flush_buffers
#cgo nocallback avcodec_free_context
#cgo nocallback avcodec_get_hw_config
#cgo nocallback avcodec_open2
#cgo nocallback avcodec_receive_frame
#cgo nocallback avcodec_send_packet
#cgo nocallback av_buffer_ref
#cgo nocallback av_buffer_unref
#cgo nocallback av_frame_alloc
#cgo nocallback av_frame_free
#cgo nocallback av_frame_unref
#cgo nocallback av_hwdevice_ctx_create
#cgo nocallback av_hwframe_transfer_data
#cgo nocallback av_packet_alloc
#cgo nocallback av_packet_free
#cgo nocallback grdp_find_v4l2m2m
#cgo noescape avcodec_send_packet
#cgo noescape grdp_copy_yuv420p_to_i420
#cgo noescape grdp_copy_nv12_to_i420
#cgo noescape grdp_copy_nv12
#cgo noescape grdp_yuv420p_to_bgra
#cgo noescape grdp_yuv420p_to_bgra_regions
#cgo noescape grdp_nv12_to_bgra
#cgo noescape grdp_nv12_to_bgra_regions
#cgo noescape grdp_frame_to_bgra
#cgo noescape grdp_sample_nv12
#cgo noescape grdp_sample_yuv
#cgo noescape grdp_sample_nv12_at
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/hwcontext.h>
#include <libavutil/log.h>
#include <libswscale/swscale.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#ifdef __ARM_NEON__
#include <arm_neon.h>
#endif

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

// grdp_set_low_delay enables AV_CODEC_FLAG_LOW_DELAY on the codec context
// so the decoder emits frames as soon as they are decoded, without waiting
// to reorder B-frames.  RDP H.264 streams transmit in display order and do
// not use B-frame reordering, so the default reorder buffer only adds
// apparent latency and (on VideoToolbox) makes legitimate frames look like
// "null frames" to our stall detector, triggering spurious hard resets.
static void grdp_set_low_delay(AVCodecContext *ctx) {
    ctx->flags |= AV_CODEC_FLAG_LOW_DELAY;
    ctx->flags2 |= AV_CODEC_FLAG2_FAST;
}

static void grdp_set_hw_pix_fmt(AVCodecContext *ctx, enum AVPixelFormat fmt) {
    ctx->opaque = (void*)(intptr_t)fmt;
}

// grdp_hwframe_map attempts a zero-copy CPU mapping of a hardware frame.
// On VideoToolbox (macOS), decoded frames live in IOSurface-backed memory that
// is accessible from both CPU and GPU.  av_hwframe_map creates a mapped view
// without copying the pixel data, allowing NV12 extraction without the extra
// GPU→RAM copy that av_hwframe_transfer_data would perform.
// Returns 0 on success; callers must fall back to av_hwframe_transfer_data
// on negative return (hardware type does not support mapping).
static int grdp_hwframe_map(AVFrame *dst, const AVFrame *src) {
    return av_hwframe_map(dst, src, AV_HWFRAME_MAP_READ);
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

// grdp_bt601_pixel writes one BGRA pixel using BT.601 coefficients.
// u and v are pre-offset (i.e. raw_value - 128).
// full_range: 0 = limited (video) range [16-235 / 16-240],
//             1 = full range [0-255].
#define CLAMP8(x) ((x) < 0 ? 0 : (x) > 255 ? 255 : (uint8_t)(x))
static inline void grdp_bt601_pixel(
    int y_raw, int u, int v, int full_range, uint8_t *dst)
{
    int r, g, b;
    if (full_range) {
        int y = y_raw;
        r = (256*y + 359*v           + 128) >> 8;
        g = (256*y -  88*u - 183*v   + 128) >> 8;
        b = (256*y + 454*u           + 128) >> 8;
    } else {
        int c = y_raw - 16;
        r = (298*c + 409*v           + 128) >> 8;
        g = (298*c - 100*u - 208*v   + 128) >> 8;
        b = (298*c + 516*u           + 128) >> 8;
    }
    dst[0] = CLAMP8(b);
    dst[1] = CLAMP8(g);
    dst[2] = CLAMP8(r);
    dst[3] = 255;
}

// grdp_yuv420p_to_bgra converts a planar YUV420P/YUVJ420P frame to packed
// BGRA using BT.601 coefficients.  This bypasses swscale entirely so that
// the broken ARM64 colorspace-matrix fallback path is never taken.
#ifdef __ARM_NEON__
// grdp_yuv420p_to_bgra_neon_8 processes 8 luma pixels (4 UV pairs) per call.
// For YUV420P each UV sample covers 2 horizontal luma pixels; we load 4 U and
// 4 V bytes and duplicate each with vzip to produce 8 per-pixel U/V vectors,
// then follow the same NEON arithmetic path as grdp_nv12_to_bgra_neon_8.
static inline void grdp_yuv420p_to_bgra_neon_8(
    const uint8_t *yrow, const uint8_t *urow, const uint8_t *vrow,
    uint8_t *drow, int col,
    int16_t ky, int16_t kr, int16_t kgu, int16_t kgv, int16_t kb,
    int16_t yoff)
{
    // Load 8 luma bytes, convert to int16, subtract Y offset (16 or 0).
    uint8x8_t y_u8 = vld1_u8(yrow + col);
    int16x8_t c16  = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(y_u8)),
                                vdupq_n_s16(yoff));

    // Load 4 U and 4 V bytes (one UV pair per 2 luma pixels).
    // vzip duplicates each byte: [U0,U1,U2,U3,...] → [U0,U0,U1,U1,U2,U2,U3,U3].
    // ffmpeg pads AVFrame line buffers for SIMD so loading 8 bytes is safe.
    uint8x8_t u_raw = vld1_u8(urow + (col >> 1));
    uint8x8_t v_raw = vld1_u8(vrow + (col >> 1));
    uint8x8_t u8    = vzip_u8(u_raw, u_raw).val[0];
    uint8x8_t v8    = vzip_u8(v_raw, v_raw).val[0];

    int16x8_t u16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(u8)), vdupq_n_s16(128));
    int16x8_t v16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(v8)), vdupq_n_s16(128));

    // Compute R/G/B with int32 to avoid overflow.  Process 4+4 pixels.
    int16x4_t c_lo = vget_low_s16(c16),  u_lo = vget_low_s16(u16),  v_lo = vget_low_s16(v16);
    int16x4_t c_hi = vget_high_s16(c16), u_hi = vget_high_s16(u16), v_hi = vget_high_s16(v16);

    int32x4_t ky_lo = vmull_n_s16(c_lo, ky), ky_hi = vmull_n_s16(c_hi, ky);

    int32x4_t r_lo = vaddq_s32(vaddq_s32(ky_lo, vmull_n_s16(v_lo, kr)), vdupq_n_s32(128));
    int32x4_t g_lo = vaddq_s32(vsubq_s32(vsubq_s32(ky_lo, vmull_n_s16(u_lo, kgu)), vmull_n_s16(v_lo, kgv)), vdupq_n_s32(128));
    int32x4_t b_lo = vaddq_s32(vaddq_s32(ky_lo, vmull_n_s16(u_lo, kb)),  vdupq_n_s32(128));

    int32x4_t r_hi = vaddq_s32(vaddq_s32(ky_hi, vmull_n_s16(v_hi, kr)), vdupq_n_s32(128));
    int32x4_t g_hi = vaddq_s32(vsubq_s32(vsubq_s32(ky_hi, vmull_n_s16(u_hi, kgu)), vmull_n_s16(v_hi, kgv)), vdupq_n_s32(128));
    int32x4_t b_hi = vaddq_s32(vaddq_s32(ky_hi, vmull_n_s16(u_hi, kb)),  vdupq_n_s32(128));

    // Shift >>8, saturate int32→int16→uint8, store interleaved BGRA.
    uint8x8_t r = vqmovun_s16(vcombine_s16(vqmovn_s32(vshrq_n_s32(r_lo,8)), vqmovn_s32(vshrq_n_s32(r_hi,8))));
    uint8x8_t g = vqmovun_s16(vcombine_s16(vqmovn_s32(vshrq_n_s32(g_lo,8)), vqmovn_s32(vshrq_n_s32(g_hi,8))));
    uint8x8_t b = vqmovun_s16(vcombine_s16(vqmovn_s32(vshrq_n_s32(b_lo,8)), vqmovn_s32(vshrq_n_s32(b_hi,8))));
    uint8x8x4_t bgra;
    bgra.val[0] = b;
    bgra.val[1] = g;
    bgra.val[2] = r;
    bgra.val[3] = vdup_n_u8(255);
    vst4_u8(drow + col * 4, bgra);
}
#endif // __ARM_NEON__

static void grdp_yuv420p_to_bgra(
    const AVFrame *src, uint8_t *dst, int dst_stride, int full_range)
{
    int width  = src->width;
    int height = src->height;
#ifdef __ARM_NEON__
    int16_t ky   = full_range ? 256 : 298;
    int16_t kr   = full_range ? 359 : 409;
    int16_t kgu  = full_range ?  88 : 100;
    int16_t kgv  = full_range ? 183 : 208;
    int16_t kb   = full_range ? 454 : 516;
    int16_t yoff = full_range ?   0 :  16;
    for (int row = 0; row < height; row++) {
        const uint8_t *yrow = src->data[0] + row        * src->linesize[0];
        const uint8_t *urow = src->data[1] + (row >> 1) * src->linesize[1];
        const uint8_t *vrow = src->data[2] + (row >> 1) * src->linesize[2];
        uint8_t *drow = dst + row * dst_stride;
        int col = 0;
        for (; col + 7 < width; col += 8)
            grdp_yuv420p_to_bgra_neon_8(yrow, urow, vrow, drow, col,
                                        ky, kr, kgu, kgv, kb, yoff);
        // Scalar tail for widths not a multiple of 8.
        for (; col < width; col++) {
            int u = (int)urow[col >> 1] - 128;
            int v = (int)vrow[col >> 1] - 128;
            grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
        }
    }
#else
    for (int row = 0; row < height; row++) {
        const uint8_t *yrow = src->data[0] + row        * src->linesize[0];
        const uint8_t *urow = src->data[1] + (row >> 1) * src->linesize[1];
        const uint8_t *vrow = src->data[2] + (row >> 1) * src->linesize[2];
        uint8_t *drow = dst + row * dst_stride;
        for (int col = 0; col < width; col++) {
            int u = (int)urow[col >> 1] - 128;
            int v = (int)vrow[col >> 1] - 128;
            grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
        }
    }
#endif
}

// grdp_yuv420p_to_bgra_regions is the region-aware variant of
// grdp_yuv420p_to_bgra.  Only pixels within the n_rects dirty rectangles
// (flat array of [left,top,right,bottom] uint16 tuples) are written to dst;
// all other pixels are left untouched, saving work proportional to the
// fraction of the frame that did not change.
static void grdp_yuv420p_to_bgra_regions(
    const AVFrame *src, uint8_t *dst, int dst_stride, int full_range,
    const uint16_t *rects, int n_rects)
{
    int width  = src->width;
    int height = src->height;
#ifdef __ARM_NEON__
    int16_t ky   = full_range ? 256 : 298;
    int16_t kr   = full_range ? 359 : 409;
    int16_t kgu  = full_range ?  88 : 100;
    int16_t kgv  = full_range ? 183 : 208;
    int16_t kb   = full_range ? 454 : 516;
    int16_t yoff = full_range ?   0 :  16;
#endif
    for (int i = 0; i < n_rects; i++) {
        int left   = (int)rects[i*4+0];
        int top    = (int)rects[i*4+1];
        int right  = (int)rects[i*4+2];
        int bottom = (int)rects[i*4+3];
        if (left   < 0)      left   = 0;
        if (top    < 0)      top    = 0;
        if (right  > width)  right  = width;
        if (bottom > height) bottom = height;
        if (left >= right || top >= bottom) continue;
        for (int row = top; row < bottom; row++) {
            const uint8_t *yrow = src->data[0] + row        * src->linesize[0];
            const uint8_t *urow = src->data[1] + (row >> 1) * src->linesize[1];
            const uint8_t *vrow = src->data[2] + (row >> 1) * src->linesize[2];
            uint8_t *drow = dst + row * dst_stride;
            int col = left;
#ifdef __ARM_NEON__
            // Advance scalar to the next multiple-of-8 boundary before the
            // NEON loop.  The NEON helper loads 8 luma bytes and 8 UV bytes
            // starting at col; col must be even for correct NV12 UV pairing.
            // A multiple of 8 satisfies both that and the 8-pixel alignment.
            int neon_start = (col + 7) & ~7;
            for (; col < neon_start && col < right; col++) {
                int u = (int)urow[col >> 1] - 128;
                int v = (int)vrow[col >> 1] - 128;
                grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
            }
            for (; col + 7 < right; col += 8)
                grdp_yuv420p_to_bgra_neon_8(yrow, urow, vrow, drow, col,
                                             ky, kr, kgu, kgv, kb, yoff);
#endif
            for (; col < right; col++) {
                int u = (int)urow[col >> 1] - 128;
                int v = (int)vrow[col >> 1] - 128;
                grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
            }
        }
    }
}

// grdp_nv12_to_bgra converts a semi-planar NV12 frame (Y plane + interleaved
// UV plane) to packed BGRA using BT.601 coefficients.  This bypasses swscale
// for the same reason as grdp_yuv420p_to_bgra: on ARM64 swscale's
// non-accelerated NV12→BGRA fallback ignores sws_setColorspaceDetails.
// VideoToolbox (macOS HW decoder) always outputs NV12.
//
// On ARM64 the inner loop is NEON-accelerated (8 pixels per iteration) to
// reduce per-frame CPU cost and decode-loop jitter.
#ifdef __ARM_NEON__
// grdp_nv12_to_bgra_neon_8 processes 8 luma pixels (4 UV pairs) per call.
// All int32x4_t intermediates prevent overflow of e.g. 298*239 = 71 222.
static inline void grdp_nv12_to_bgra_neon_8(
    const uint8_t *yrow, const uint8_t *uvrow, uint8_t *drow,
    int col, int16_t ky, int16_t kr, int16_t kgu, int16_t kgv, int16_t kb,
    int16_t yoff)
{
    // Load 8 luma bytes, convert to int16, subtract Y offset (16 or 0).
    uint8x8_t y_u8 = vld1_u8(yrow + col);
    int16x8_t c16  = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(y_u8)),
                                vdupq_n_s16(yoff));

    // Load 8 UV bytes: [U0,V0,U1,V1,U2,V2,U3,V3].
    // vuzp deinterleaves into .val[0]=[U0,U1,U2,U3,…] .val[1]=[V0,V1,V2,V3,…].
    // vzip then duplicates each value for the two luma pixels it serves.
    uint8x8_t uv_u8    = vld1_u8(uvrow + col);
    uint8x8x2_t uv_sep = vuzp_u8(uv_u8, uv_u8);
    uint8x8_t u8       = vzip_u8(uv_sep.val[0], uv_sep.val[0]).val[0]; // [U0,U0,U1,U1,…]
    uint8x8_t v8       = vzip_u8(uv_sep.val[1], uv_sep.val[1]).val[0]; // [V0,V0,V1,V1,…]

    int16x8_t u16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(u8)), vdupq_n_s16(128));
    int16x8_t v16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(v8)), vdupq_n_s16(128));

    // Compute R/G/B with int32 to avoid overflow.  Process 4+4 pixels.
    int16x4_t c_lo = vget_low_s16(c16),  u_lo = vget_low_s16(u16),  v_lo = vget_low_s16(v16);
    int16x4_t c_hi = vget_high_s16(c16), u_hi = vget_high_s16(u16), v_hi = vget_high_s16(v16);

    int32x4_t ky_lo = vmull_n_s16(c_lo, ky), ky_hi = vmull_n_s16(c_hi, ky);

    int32x4_t r_lo = vaddq_s32(vaddq_s32(ky_lo, vmull_n_s16(v_lo, kr)), vdupq_n_s32(128));
    int32x4_t g_lo = vaddq_s32(vsubq_s32(vsubq_s32(ky_lo, vmull_n_s16(u_lo, kgu)), vmull_n_s16(v_lo, kgv)), vdupq_n_s32(128));
    int32x4_t b_lo = vaddq_s32(vaddq_s32(ky_lo, vmull_n_s16(u_lo, kb)),  vdupq_n_s32(128));

    int32x4_t r_hi = vaddq_s32(vaddq_s32(ky_hi, vmull_n_s16(v_hi, kr)), vdupq_n_s32(128));
    int32x4_t g_hi = vaddq_s32(vsubq_s32(vsubq_s32(ky_hi, vmull_n_s16(u_hi, kgu)), vmull_n_s16(v_hi, kgv)), vdupq_n_s32(128));
    int32x4_t b_hi = vaddq_s32(vaddq_s32(ky_hi, vmull_n_s16(u_hi, kb)),  vdupq_n_s32(128));

    // Shift >>8, saturate int32→int16→uint8, then store interleaved BGRA.
    uint8x8_t r = vqmovun_s16(vcombine_s16(vqmovn_s32(vshrq_n_s32(r_lo,8)), vqmovn_s32(vshrq_n_s32(r_hi,8))));
    uint8x8_t g = vqmovun_s16(vcombine_s16(vqmovn_s32(vshrq_n_s32(g_lo,8)), vqmovn_s32(vshrq_n_s32(g_hi,8))));
    uint8x8_t b = vqmovun_s16(vcombine_s16(vqmovn_s32(vshrq_n_s32(b_lo,8)), vqmovn_s32(vshrq_n_s32(b_hi,8))));
    uint8x8x4_t bgra;
    bgra.val[0] = b;
    bgra.val[1] = g;
    bgra.val[2] = r;
    bgra.val[3] = vdup_n_u8(255);
    vst4_u8(drow + col * 4, bgra);
}
#endif // __ARM_NEON__

static void grdp_nv12_to_bgra(
    const AVFrame *src, uint8_t *dst, int dst_stride, int full_range)
{
    int width  = src->width;
    int height = src->height;
#ifdef __ARM_NEON__
    // NEON fast path: 8 pixels per inner iteration on ARM64.
    int16_t ky  = full_range ? 256 : 298;
    int16_t kr  = full_range ? 359 : 409;
    int16_t kgu = full_range ?  88 : 100;
    int16_t kgv = full_range ? 183 : 208;
    int16_t kb  = full_range ? 454 : 516;
    int16_t yoff = full_range ? 0 : 16;
    for (int row = 0; row < height; row++) {
        const uint8_t *yrow  = src->data[0] + row        * src->linesize[0];
        const uint8_t *uvrow = src->data[1] + (row >> 1) * src->linesize[1];
        uint8_t *drow = dst + row * dst_stride;
        int col = 0;
        for (; col + 7 < width; col += 8)
            grdp_nv12_to_bgra_neon_8(yrow, uvrow, drow, col, ky, kr, kgu, kgv, kb, yoff);
        // Scalar tail for widths not a multiple of 8.
        for (; col < width; col++) {
            int u = (int)uvrow[(col >> 1) * 2    ] - 128;
            int v = (int)uvrow[(col >> 1) * 2 + 1] - 128;
            grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
        }
    }
#else
    for (int row = 0; row < height; row++) {
        const uint8_t *yrow  = src->data[0] + row        * src->linesize[0];
        const uint8_t *uvrow = src->data[1] + (row >> 1) * src->linesize[1];
        uint8_t *drow = dst + row * dst_stride;
        for (int col = 0; col < width; col++) {
            int u = (int)uvrow[(col >> 1) * 2    ] - 128;
            int v = (int)uvrow[(col >> 1) * 2 + 1] - 128;
            grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
        }
    }
#endif
}

// grdp_nv12_to_bgra_regions is the region-aware variant of grdp_nv12_to_bgra.
// Only pixels within the n_rects dirty rectangles (flat [left,top,right,bottom]
// uint16 tuples) are written; all other pixels in dst are left untouched.
static void grdp_nv12_to_bgra_regions(
    const AVFrame *src, uint8_t *dst, int dst_stride, int full_range,
    const uint16_t *rects, int n_rects)
{
    int width  = src->width;
    int height = src->height;
#ifdef __ARM_NEON__
    int16_t ky   = full_range ? 256 : 298;
    int16_t kr   = full_range ? 359 : 409;
    int16_t kgu  = full_range ?  88 : 100;
    int16_t kgv  = full_range ? 183 : 208;
    int16_t kb   = full_range ? 454 : 516;
    int16_t yoff = full_range ?   0 :  16;
#endif
    for (int i = 0; i < n_rects; i++) {
        int left   = (int)rects[i*4+0];
        int top    = (int)rects[i*4+1];
        int right  = (int)rects[i*4+2];
        int bottom = (int)rects[i*4+3];
        if (left   < 0)      left   = 0;
        if (top    < 0)      top    = 0;
        if (right  > width)  right  = width;
        if (bottom > height) bottom = height;
        if (left >= right || top >= bottom) continue;
        for (int row = top; row < bottom; row++) {
            const uint8_t *yrow  = src->data[0] + row        * src->linesize[0];
            const uint8_t *uvrow = src->data[1] + (row >> 1) * src->linesize[1];
            uint8_t *drow = dst + row * dst_stride;
            int col = left;
#ifdef __ARM_NEON__
            int neon_start = (col + 7) & ~7;
            for (; col < neon_start && col < right; col++) {
                int u = (int)uvrow[(col >> 1) * 2    ] - 128;
                int v = (int)uvrow[(col >> 1) * 2 + 1] - 128;
                grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
            }
            for (; col + 7 < right; col += 8)
                grdp_nv12_to_bgra_neon_8(yrow, uvrow, drow, col, ky, kr, kgu, kgv, kb, yoff);
#endif
            for (; col < right; col++) {
                int u = (int)uvrow[(col >> 1) * 2    ] - 128;
                int v = (int)uvrow[(col >> 1) * 2 + 1] - 128;
                grdp_bt601_pixel((int)yrow[col], u, v, full_range, drow + col*4);
            }
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

// grdp_sample_nv12 samples the centre pixel of a semi-planar NV12 frame
// (Y plane + interleaved UV plane) for diagnostics.
static void grdp_sample_nv12(const AVFrame *f,
    uint8_t *y_out, uint8_t *u_out, uint8_t *v_out)
{
    int cx = f->width  / 2;
    int cy = f->height / 2;
    *y_out = f->data[0][cy * f->linesize[0] + cx];
    int uvx = (cx / 2) * 2; // NV12: interleaved, U at even index, V at odd
    int uvy = cy / 2;
    *u_out = f->data[1][uvy * f->linesize[1] + uvx];
    *v_out = f->data[1][uvy * f->linesize[1] + uvx + 1];
}

// grdp_sample_nv12_at samples a specific (x,y) pixel from an NV12 frame.
static void grdp_sample_nv12_at(const AVFrame *f, int x, int y,
    uint8_t *y_out, uint8_t *u_out, uint8_t *v_out)
{
    if (x < 0 || x >= f->width || y < 0 || y >= f->height) {
        *y_out = *u_out = *v_out = 0;
        return;
    }
    *y_out = f->data[0][y * f->linesize[0] + x];
    int uvx = (x >> 1) * 2;
    int uvy = y >> 1;
    *u_out = f->data[1][uvy * f->linesize[1] + uvx];
    *v_out = f->data[1][uvy * f->linesize[1] + uvx + 1];
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

// grdp_copy_yuv420p_to_i420 copies an AVFrame in YUV420P or YUVJ420P format
// to tightly-packed I420 planes (stride = width for Y, stride = (width+1)/2 for U/V).
// ydst, udst, vdst must be pre-allocated by the caller.
// When the source frame is tight-packed (linesize == stride), a single bulk
// memcpy is used per plane instead of per-row copies.
static void grdp_copy_yuv420p_to_i420(
    const AVFrame *f,
    uint8_t *ydst, uint8_t *udst, uint8_t *vdst,
    int w, int h)
{
    int pw = (w + 1) / 2;
    int ph = (h + 1) / 2;
    if (f->linesize[0] == w)
        memcpy(ydst, f->data[0], (size_t)w * h);
    else
        for (int y = 0; y < h; y++)
            memcpy(ydst + y * w, f->data[0] + y * f->linesize[0], w);
    if (f->linesize[1] == pw)
        memcpy(udst, f->data[1], (size_t)pw * ph);
    else
        for (int y = 0; y < ph; y++)
            memcpy(udst + y * pw, f->data[1] + y * f->linesize[1], pw);
    if (f->linesize[2] == pw)
        memcpy(vdst, f->data[2], (size_t)pw * ph);
    else
        for (int y = 0; y < ph; y++)
            memcpy(vdst + y * pw, f->data[2] + y * f->linesize[2], pw);
}

// grdp_copy_nv12_to_i420 copies an AVFrame in NV12 format (Y plane + interleaved UV)
// to tightly-packed I420 planes.
// The Y plane is bulk-copied when tight-packed.
// On ARM64 the UV deinterleave loop uses NEON vld2q_u8 to process 16 chroma
// pairs per iteration, roughly halving the cost of the chroma plane copy.
static void grdp_copy_nv12_to_i420(
    const AVFrame *f,
    uint8_t *ydst, uint8_t *udst, uint8_t *vdst,
    int w, int h)
{
    int pw = (w + 1) / 2;
    int ph = (h + 1) / 2;
    if (f->linesize[0] == w)
        memcpy(ydst, f->data[0], (size_t)w * h);
    else
        for (int y = 0; y < h; y++)
            memcpy(ydst + y * w, f->data[0] + y * f->linesize[0], w);
    for (int y = 0; y < ph; y++) {
        const uint8_t *row = f->data[1] + y * f->linesize[1];
        uint8_t *ud = udst + y * pw;
        uint8_t *vd = vdst + y * pw;
        int x = 0;
#ifdef __ARM_NEON__
        for (; x + 15 < pw; x += 16) {
            uint8x16x2_t uv = vld2q_u8(row + x * 2);
            vst1q_u8(ud + x, uv.val[0]);
            vst1q_u8(vd + x, uv.val[1]);
        }
#endif
        for (; x < pw; x++) {
            ud[x] = row[x * 2];
            vd[x] = row[x * 2 + 1];
        }
    }
}

// grdp_copy_nv12 copies an AVFrame in NV12 format to tightly-packed NV12
// planes.  Unlike grdp_copy_nv12_to_i420, this keeps the interleaved UV plane
// intact so SDL2 can upload it via SDL_UpdateNVTexture without CPU-side
// deinterleaving.
// When the source frame is tight-packed (linesize == stride), a single bulk
// memcpy is used per plane instead of per-row copies.
static void grdp_copy_nv12(
    const AVFrame *f,
    uint8_t *ydst, uint8_t *uvdst,
    int w, int h)
{
    int uv_bytes = ((w + 1) / 2) * 2;
    int ph = (h + 1) / 2;
    if (f->linesize[0] == w)
        memcpy(ydst, f->data[0], (size_t)w * h);
    else
        for (int y = 0; y < h; y++)
            memcpy(ydst + y * w, f->data[0] + y * f->linesize[0], w);
    if (f->linesize[1] == uv_bytes)
        memcpy(uvdst, f->data[1], (size_t)uv_bytes * ph);
    else
        for (int y = 0; y < ph; y++)
            memcpy(uvdst + y * uv_bytes, f->data[1] + y * f->linesize[1], uv_bytes);
}

// grdp_find_v4l2m2m returns the h264_v4l2m2m decoder if FFmpeg was compiled
// with V4L2 M2M support (common on Linux/Raspberry Pi). Returns NULL otherwise.
static const AVCodec *grdp_find_v4l2m2m(void) {
    return avcodec_find_decoder_by_name("h264_v4l2m2m");
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/nakagami/grdp/plugin/rdpgfx"
)

// useSwscale controls whether YUV420P/YUVJ420P and NV12 frames are converted
// to BGRA via swscale (SIMD-accelerated on x86_64) or via hand-written C loops.
// On ARM64, swscale's non-accelerated paths ignore sws_setColorspaceDetails,
// producing a strong green cast; the hand-written BT.601 loops are used instead.
// On x86_64, swscale is both correct and significantly faster (SSSE3/AVX2).
var useSwscale = runtime.GOARCH != "arm64"

// avLogOnce ensures grdp_suppress_av_log is called only once per process.
var avLogOnce sync.Once

// avcFreezeThreshold is the duration of no decoded output from the HW decoder
// after which it is marked broken.  The application-level watchdog then
// reconnects the RDP session.  VideoToolbox (macOS) can stall for 2-3 seconds
// while processing a new IDR/SPS frame; 6 seconds gives it enough headroom to
// recover naturally before we declare it broken.  FreeRDP takes a similar
// passive approach: it drops failed frames without hard resets or IDR requests
// and waits for the server to resume naturally.
// This threshold applies to the initial-stall case (hwReady=false).
const avcFreezeThreshold = 6 * time.Second

// avcHWReadyFreezeThreshold is the point at which a stalled HW decoder stops
// accepting new packets.  VideoToolbox can legitimately pause for several
// seconds when flushing its internal reference pipeline at an IDR/GOP
// boundary; 7 s is chosen because the CGo call (avcodec_send_packet) itself
// permanently blocks after ~5.75 s of stall on macOS VideoToolbox.  The
// pre-flight guard in Decode() bails out *before* the CGo call to prevent the
// decodeLoop goroutine from hanging inside CGo.
//
// Crossing this threshold does NOT immediately mark the decoder broken.
// Instead Decode() enters a recovery-probe window (avcHWRecoveryWindow) and
// keeps probing avcodec_receive_frame without sending new packets.  If
// VideoToolbox produces a frame during that window the stall clock is reset
// and normal decoding resumes; only if the window is exhausted is the decoder
// marked broken.
const avcHWReadyFreezeThreshold = 7 * time.Second

// avcHWEarlyFreezeThreshold is a shorter stall threshold applied during the
// first avcHWEarlyFrameLimit packets sent to the HW decoder after each
// decoder initialisation or flush.  VideoToolbox exhibits a characteristic
// stall pattern at RDP session start (and after a forced flush): it processes
// a small burst of frames, then freezes for 8+ seconds without recovering.
// The normal 7 s threshold is designed for mid-session IDR stalls that
// self-resolve in 2-3 s; in the early phase a genuine VT stall is
// distinguishable because it persists well beyond 5 s.  Using a shorter
// threshold here reduces the visible freeze by ~2 s while avoiding false
// positives from transient 3-4 s null-frame bursts at IDR/GOP boundaries.
const avcHWEarlyFreezeThreshold = 5 * time.Second

// avcHWEarlyFrameLimit is the number of packets sent to the HW decoder
// (hwSentCount) below which avcHWEarlyFreezeThreshold is used instead of
// avcHWReadyFreezeThreshold.  hwSentCount resets to zero on every
// avcodec_send_packet failure (decoder flush), so this threshold also covers
// the early window after an in-session flush.  50 packets at 30 fps ≈ 1.7 s,
// comfortably covering the unstable session-start window without interfering
// with normal mid-session IDR stalls.
const avcHWEarlyFrameLimit = 50

// avcHWRecoveryWindow is how long Decode() probes for pending output after
// a stall is detected (either from avcHWReadyFreezeThreshold or from the
// early null-frame count detector).  After this window, if VT has not
// produced a real frame, the decoder is declared broken and a soft reset
// is triggered.  500 ms is sufficient: any frame VT had buffered will have
// surfaced well within 500 ms, and YouTube / gnome-remote-desktop delivers a
// fresh IDR within ~2 s of the soft reset anyway.
const avcHWRecoveryWindow = 500 * time.Millisecond

// avcHWNullFrameStallLimit is the number of consecutive null (blank) frames
// the HW decoder may produce during the early session window (hwSentCount <
// avcHWEarlyFrameLimit) before triggering a stall-probe.  VideoToolbox
// legitimately outputs a handful of null frames at IDR boundaries during
// session start (observed: 5–25 frames / up to ~1 s at 30 fps).
const avcHWNullFrameStallLimit = 25

// avcHWMidSessionNullFrameLimit is the null-frame count threshold used for
// mid-session stall detection (hwSentCount >= avcHWEarlyFrameLimit).
// Normal GOP / mid-session IDR boundaries produce at most ~25 null frames
// (~1 s at 30 fps); a genuine VideoToolbox stall produces hundreds.  150
// frames ≈ 5 s at 30 fps — well above normal GOP noise but 2 s before the
// 7-second safety valve, reducing visible freeze duration by ~2 s.
const avcHWMidSessionNullFrameLimit = 150

// keyframeWaitLimit is the maximum number of non-IDR packets we drop while
// waiting for a keyframe after a decoder reset or flush.  gnome-remote-desktop
// and similar servers send an IDR approximately every 15-25 seconds; using 900
// frames (~30s at 30 fps) ensures we catch the next natural IDR even under
// variable server GOP intervals.  After this limit the SW decoder attempts
// error-concealment decode; the HW decoder marks itself broken instead (HW
// codecs like VideoToolbox cannot recover without a proper IDR).
const keyframeWaitLimit = 900

// keyframeWaitTimeout is the maximum wall-clock time an HW decoder waits for
// an IDR after entering needsKeyFrame=true.  ForceRefresh is sent every 2 s,
// so the server should respond within a few seconds.  If no IDR arrives within
// this window the HW decoder marks itself broken so the soft-reset / reconnect
// chain escalates quickly rather than waiting the full keyframeWaitLimit
// (~30 s) of dropped packets.  The HW path cannot do error-concealment, so a
// longer window gives the server more chances to deliver a natural IDR.
const keyframeWaitTimeout = 15 * time.Second

// keyframeWaitTimeoutSW is the keyframe-wait wall-clock limit for detached SW
// decoders (aux decoder h264dec2, no watchdog channel).  These decoders do not
// have an external timer to terminate the wait, so we attempt error-concealment
// sooner and let the caller tear down and recreate the decoder on the next IDR.
const keyframeWaitTimeoutSW = 5 * time.Second

// auxEAGAINThreshold is the number of consecutive EAGAIN results from h264dec2
// (aux SW decoder, no watchdog channel) before it is marked broken.  After a
// flush the existing torn-down/recreate-on-IDR machinery in avc.go rebuilds
// the decoder from the next stream2 IDR without requiring a full reconnect.
// Stream2 data appears in ~1–2 % of LC=0 packets (~0.5 events/s at 30 fps),
// so 10 events correspond to roughly 5–20 s of visible stale chroma — much
// better than the previous ~3-minute wait for the session reconnect.
const auxEAGAINThreshold = 10

// keyframeWaitTimeoutSWFallback is the keyframe-wait timer for the main SW
// fallback decoder (created after a VideoToolbox stall).  By the time SW
// fallback is activated, the proactive ForceRefresh mechanism has already been
// sending keyframe requests for ~7 s.  Windows servers consistently do not
// deliver an IDR in response to keyframe requests post-stall; only a full
// reconnect forces a new IDR.  A short window here triggers reconnect quickly
// (total freeze ≈ 7 s VT stall + 3 s IDR wait + ~4 s reconnect ≈ 14 s total)
// rather than waiting the full 8 s (19 s total).
const keyframeWaitTimeoutSWFallback = 1 * time.Second

// profileWindow is the number of HW frames over which Decode aggregates
// timing measurements before logging an INFO summary.  At 30 fps this is
// roughly one log line every ~10 s.
const profileWindow = 300

type ffmpegDecoder struct {
	codecCtx                 *C.AVCodecContext
	packet                   *C.AVPacket
	frame                    *C.AVFrame
	swFrame                  *C.AVFrame
	mapFrame                 *C.AVFrame // reusable frame for av_hwframe_map zero-copy transfers
	hwMapSupported           int8       // 0=unknown, 1=supported, -1=unsupported
	swsCtx                   *C.struct_SwsContext
	useHW                    bool
	hwPixFmt                 C.enum_AVPixelFormat
	lastW                    C.int
	lastH                    C.int
	lastFmt                  C.enum_AVPixelFormat
	lastFullRange            C.int     // tracks fullRange used when swsCtx was last configured
	lastSuccessTime          time.Time // wall-clock time of the last successfully decoded frame
	lastSendTime             time.Time // wall-clock time of the last avcodec_send_packet call
	lastReceiveTime          time.Time // wall-clock time of the last Decode() call (updated on every call)
	hwFirstSendTime          time.Time // wall-clock time of the first packet sent to the HW decoder
	needsKeyFrame            bool      // drop packets until an IDR/SPS is received
	keyframeWaitCount        int       // P-frames dropped so far while needsKeyFrame=true
	keyframeWaitStart        time.Time // wall-clock time of the first dropped P-frame while waiting for IDR
	hwReady                  bool      // HW decoder has produced at least one frame
	hwSentCount              int       // packets sent to HW decoder (for diagnostics)
	swFrameCount             int       // frames decoded by SW decoder (for diagnostics)
	hwFrameCount             int       // frames decoded by HW decoder (for diagnostics)
	broken                   bool      // decoder is unrecoverable; stop producing frames so the app reconnects
	brokenReason             rdpgfx.H264BrokenReason
	timerBroken              atomic.Bool // set by background timers when probe/IDR timeouts expire
	timerBrokenReason        atomic.Int32
	proceededWithoutKeyframe bool            // "proceed without keyframe" path was taken; AVERROR here means broken
	stallProbeStart          time.Time       // wall-clock time we entered the stall recovery-probe window
	stallTimer               *time.Timer     // fires after avcHWRecoveryWindow to mark broken independently of frame rate
	hwConsecNullFrames       int             // consecutive HW null frames since last real frame; for early stall detection
	kfWaitTimer              *time.Timer     // fires after kfWaitTimeoutVal to mark broken independently of frame rate
	kfWaitTimeoutVal         time.Duration   // per-decoder IDR wait limit (varies by decoder type)
	watchdogCh               chan<- struct{} // signals GfxHandler.decodeLoop to call maybeNotifyDecoderBroken
	auxEAGAINCount           int            // consecutive EAGAINs from h264dec2; reset on success, markBroken at threshold
	auxEAGAINFlush           bool           // set after EAGAIN flush; suppresses error-concealment until stream2 IDR arrives

	// Profiling: aggregated timing stats over the last profileWindow frames
	// for the HW path.  Helps determine whether convertFrame
	// (av_hwframe_transfer_data + colour conversion) is the bottleneck that
	// causes VideoToolbox to stall by holding GPU frames too long.
	profFrames     int
	profSendNs     int64 // total ns in avcodec_send_packet
	profRecvNs     int64 // total ns in avcodec_receive_frame loop (excluding convert)
	profConvertNs  int64 // total ns in convertFrame (transfer + colour conversion)
	profTransferNs int64 // total ns in av_hwframe_transfer_data only
	profMaxConvNs  int64 // worst-case convertFrame duration in window
	profMaxSendNs  int64 // worst-case avcodec_send_packet duration in window
	profMaxRecvNs  int64 // worst-case avcodec_receive_frame duration in window

	// outRing holds two recyclable BGRA destination buffers.  convertFrame
	// rotates between them so each Decode() avoids allocating a fresh
	// width*height*4 buffer (≈8MB at 1920×1080 → ≈240MB/s of GC garbage at
	// 30fps).  Two slots is sufficient because emitBitmap is called
	// synchronously from the rdpgfx PDU loop and always finishes (the
	// caller has copied the data into its backing image) before the next
	// Decode runs.  outRingIdx selects the slot to use *next*.
	outRing    [2][]byte
	outRingIdx int

	// outI420Ring holds two recyclable I420 frame slots for GPU-accelerated
	// rendering via SDL2 IYUV textures.  Same ring/lifecycle pattern as outRing.
	// outI420Enabled gates I420 extraction (set by DecodeWithI420); lastI420
	// is the result from the most recent convertFrame call.
	outI420Ring    [2]rdpgfx.H264FrameI420
	outI420RingIdx int
	outI420Enabled bool
	lastI420       *rdpgfx.H264FrameI420

	// outNV12Ring holds native NV12 frames for SDL2 NV12 texture upload.
	// This path is especially useful for VideoToolbox, whose transferred
	// software frames are usually NV12.
	outNV12Ring    [2]rdpgfx.H264FrameNV12
	outNV12RingIdx int
	outNV12Enabled bool
	lastNV12       *rdpgfx.H264FrameNV12

	// regionHint carries dirty-rect hints for region-aware YUV→BGRA conversion.
	// setRegionHint populates these fields; Decode() captures them into local
	// variables at entry (clearing nRegionHints) so stale hints can never
	// carry over to a subsequent unrelated frame.
	regionHint   []C.uint16_t // flat [left,top,right,bottom,...] per rect
	nRegionHints C.int        // number of valid rects in regionHint

	// hwNeedsZeroCheck is set to true on decoder creation and after each
	// avcodec_flush_buffers call.  When set, convertFrame checks the first
	// NV12 output frame for a zero-filled chroma plane (U=0, V=0).
	//
	// VideoToolbox sometimes returns a zero-initialised IOSurface for the
	// first decoded frame after init or a pipeline flush.  The BT.601
	// limited-range conversion of (Y=0, U=0, V=0) produces BGRA(0,135,0,255)
	// — a full-screen dark-green frame that manifests as a brief "green
	// curtain" in the UI.  Valid NV12 chroma always centres on 128
	// (limited-range [16,240], full-range centred on 128), so U=0 and V=0
	// occurring simultaneously at the centre pixel unambiguously signals an
	// uninitialised buffer rather than real video content.
	hwNeedsZeroCheck bool
}

// extractI420fromSrc extracts I420 planar data from srcFrame into the ring
// buffer and stores a pointer in d.lastI420.  Called from convertFrame() when
// outI420Enabled is true, before av_frame_unref(d.swFrame).
// Sets d.lastI420 = nil when the pixel format is not directly supported.
func (d *ffmpegDecoder) extractI420fromSrc(srcFrame *C.AVFrame) {
	srcFmt := C.enum_AVPixelFormat(srcFrame.format)
	if srcFmt != C.AV_PIX_FMT_YUV420P && srcFmt != C.AV_PIX_FMT_YUVJ420P &&
		srcFmt != C.AV_PIX_FMT_NV12 {
		d.lastI420 = nil
		return
	}

	w := int(srcFrame.width)
	h := int(srcFrame.height)
	pw := (w + 1) / 2
	ph := (h + 1) / 2
	ySize := w * h
	uvSize := pw * ph

	slot := &d.outI420Ring[d.outI420RingIdx]
	d.outI420RingIdx ^= 1

	if cap(slot.Y) < ySize {
		slot.Y = make([]byte, ySize)
	} else {
		slot.Y = slot.Y[:ySize]
	}
	if cap(slot.U) < uvSize {
		slot.U = make([]byte, uvSize)
	} else {
		slot.U = slot.U[:uvSize]
	}
	if cap(slot.V) < uvSize {
		slot.V = make([]byte, uvSize)
	} else {
		slot.V = slot.V[:uvSize]
	}
	slot.YStride = w
	slot.UStride = pw
	slot.VStride = pw
	slot.Width = w
	slot.Height = h
	slot.FullRange = srcFmt == C.AV_PIX_FMT_YUVJ420P || srcFrame.color_range == 2

	if srcFmt == C.AV_PIX_FMT_YUV420P || srcFmt == C.AV_PIX_FMT_YUVJ420P {
		C.grdp_copy_yuv420p_to_i420(srcFrame,
			(*C.uint8_t)(unsafe.Pointer(&slot.Y[0])),
			(*C.uint8_t)(unsafe.Pointer(&slot.U[0])),
			(*C.uint8_t)(unsafe.Pointer(&slot.V[0])),
			C.int(w), C.int(h))
	} else {
		C.grdp_copy_nv12_to_i420(srcFrame,
			(*C.uint8_t)(unsafe.Pointer(&slot.Y[0])),
			(*C.uint8_t)(unsafe.Pointer(&slot.U[0])),
			(*C.uint8_t)(unsafe.Pointer(&slot.V[0])),
			C.int(w), C.int(h))
	}

	d.lastI420 = slot
}

// extractNV12fromSrc copies native NV12 planes from srcFrame into the ring
// buffer and stores a pointer in d.lastNV12.  It intentionally does not
// deinterleave chroma, so SDL2 NV12 texture uploads avoid the I420 conversion
// work required by extractI420fromSrc.
func (d *ffmpegDecoder) extractNV12fromSrc(srcFrame *C.AVFrame) {
	if C.enum_AVPixelFormat(srcFrame.format) != C.AV_PIX_FMT_NV12 {
		d.lastNV12 = nil
		return
	}

	w := int(srcFrame.width)
	h := int(srcFrame.height)
	uvStride := ((w + 1) / 2) * 2
	ph := (h + 1) / 2
	ySize := w * h
	uvSize := uvStride * ph

	slot := &d.outNV12Ring[d.outNV12RingIdx]
	d.outNV12RingIdx ^= 1

	if cap(slot.Y) < ySize {
		slot.Y = make([]byte, ySize)
	} else {
		slot.Y = slot.Y[:ySize]
	}
	if cap(slot.UV) < uvSize {
		slot.UV = make([]byte, uvSize)
	} else {
		slot.UV = slot.UV[:uvSize]
	}
	slot.YStride = w
	slot.UVStride = uvStride
	slot.Width = w
	slot.Height = h
	slot.FullRange = srcFrame.color_range == 2

	C.grdp_copy_nv12(srcFrame,
		(*C.uint8_t)(unsafe.Pointer(&slot.Y[0])),
		(*C.uint8_t)(unsafe.Pointer(&slot.UV[0])),
		C.int(w), C.int(h))

	d.lastNV12 = slot
}

func newH264DecoderInternal(watchdogCh chan<- struct{}, forceSW bool, kfWaitTimeout time.Duration) rdpgfx.H264Decoder {
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

	// alreadyOpened is set when a codec-specific path (e.g. V4L2 M2M) opens
	// its own AVCodecContext before the shared avcodec_open2 call below.
	alreadyOpened := false

	d := &ffmpegDecoder{
		codecCtx:         codecCtx,
		hwPixFmt:         C.AV_PIX_FMT_NONE,
		lastFmt:          C.AV_PIX_FMT_NONE,
		needsKeyFrame:    true, // always wait for a clean IDR before feeding packets
		hwNeedsZeroCheck: true, // check first NV12 output for zero-filled IOSurface
		watchdogCh:       watchdogCh,
		kfWaitTimeoutVal: kfWaitTimeout,
	}

	// Always enable LOW_DELAY: RDP H.264 streams are transmitted in display
	// order with no B-frame reordering, so the default reorder buffer adds
	// no value and (especially on VideoToolbox) makes the decoder appear
	// stalled between IDRs.
	C.grdp_set_low_delay(codecCtx)

	if !forceSW {
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

		// If no hwdevice backend was found, try the V4L2 M2M codec (h264_v4l2m2m).
		// This is a standalone FFmpeg codec that directly outputs NV12 frames in
		// CPU-accessible memory and is commonly available on Linux SoCs such as
		// Raspberry Pi 4/5.  It is not exposed via av_hwdevice_iterate_types and
		// must be probed explicitly.  On macOS or when FFmpeg is built without V4L2
		// support, avcodec_find_decoder_by_name returns nil and the probe is a no-op.
		if !d.useHW {
			v4l2Codec := C.grdp_find_v4l2m2m()
			if v4l2Codec != nil {
				v4l2Ctx := C.avcodec_alloc_context3(v4l2Codec)
				if v4l2Ctx != nil {
					C.grdp_set_low_delay(v4l2Ctx)
					if C.avcodec_open2(v4l2Ctx, v4l2Codec, nil) >= 0 {
						// Replace the standard h264 context with the V4L2 M2M one.
						// avcodec_free_context sets d.codecCtx to nil via its **ctx arg.
						C.avcodec_free_context(&d.codecCtx)
						d.codecCtx = v4l2Ctx
						codecCtx = v4l2Ctx
						d.useHW = true
						d.hwNeedsZeroCheck = false // no zero-filled IOSurface on V4L2
						alreadyOpened = true
						slog.Debug("H.264: V4L2 M2M hardware acceleration enabled")
					} else {
						C.avcodec_free_context(&v4l2Ctx)
					}
				}
			}
		}
	}

	if !d.useHW {
		if d.watchdogCh != nil {
			// Main decoder switching from VideoToolbox to FFmpeg after a stall.
			slog.Debug("H.264: using software decoding (SW fallback)")
		} else {
			// Aux decoder (h264dec2) or initial SW-only decoder — always pure SW.
			slog.Debug("H.264: using software decoding")
		}
		// Limit the decoded picture buffer to 1 reference frame so each frame
		// is emitted immediately rather than waiting for up to
		// max_dec_frame_buffering (often 8) frames to accumulate.  RDP H.264
		// streams use sequential P-frames that only reference the immediately
		// preceding frame, so this is safe.  VideoToolbox (HW path) has its
		// own zero-latency output mechanism and does not need this.
		codecCtx.refs = 1
		// Use slice-level threading only.  Frame-level threading (the FFmpeg
		// default) introduces a one-frame reorder delay that conflicts with
		// AV_CODEC_FLAG_LOW_DELAY and causes each decoded frame to arrive one
		// frame late — effectively doubling input latency.  Slice threading
		// parallelises within a single frame with no added latency, which is
		// beneficial when the server encodes multiple slices per frame.
		codecCtx.thread_type = C.FF_THREAD_SLICE
	}

	if !alreadyOpened {
		if C.avcodec_open2(codecCtx, codec, nil) < 0 {
			C.avcodec_free_context(&d.codecCtx)
			return nil
		}
	}

	d.packet = C.av_packet_alloc()
	d.frame = C.av_frame_alloc()
	d.swFrame = C.av_frame_alloc()
	d.mapFrame = C.av_frame_alloc()
	if d.packet == nil || d.frame == nil || d.swFrame == nil || d.mapFrame == nil {
		d.Close()
		return nil
	}

	// Arm the keyframe-wait timer immediately so recovery is triggered even
	// when the server sends no frames after a soft reset (e.g. static screen
	// or ForceRefresh ignored by the server).  If an IDR arrives first,
	// Decode() cancels the timer.  Decoders without a watchdog channel
	// (e.g. h264dec2) are not armed here — they are managed separately.
	if watchdogCh != nil {
		d.kfWaitTimer = time.AfterFunc(kfWaitTimeout, func() {
			d.timerBrokenReason.Store(int32(rdpgfx.H264BrokenReasonNoIDR))
			d.timerBroken.Store(true)
			d.signalWatchdog()
		})
	}

	runtime.SetFinalizer(d, func(dec *ffmpegDecoder) { dec.Close() })
	return d
}

func (d *ffmpegDecoder) NeedsKeyframe() bool {
	return d.needsKeyFrame
}

func (d *ffmpegDecoder) NeedsIDR() bool {
	return d.needsKeyFrame
}

func (d *ffmpegDecoder) IsBroken() bool {
	return d.broken || d.timerBroken.Load()
}

func (d *ffmpegDecoder) BrokenReason() rdpgfx.H264BrokenReason {
	if d.brokenReason != rdpgfx.H264BrokenReasonNone {
		return d.brokenReason
	}
	return rdpgfx.H264BrokenReason(d.timerBrokenReason.Load())
}

func (d *ffmpegDecoder) ForceBroken(reason rdpgfx.H264BrokenReason) {
	d.markBroken(reason)
}

// markBroken sets d.broken and stops any pending background timers.
// Called from inside Decode() (decodeLoop goroutine) when a timeout fires.
func (d *ffmpegDecoder) markBroken(reason rdpgfx.H264BrokenReason) {
	d.broken = true
	if reason != rdpgfx.H264BrokenReasonNone {
		d.brokenReason = reason
		d.timerBrokenReason.Store(int32(reason))
	}
	d.stopTimers()
}

// stopTimers cancels the stall-probe and IDR-wait background timers.
func (d *ffmpegDecoder) stopTimers() {
	if d.stallTimer != nil {
		d.stallTimer.Stop()
		d.stallTimer = nil
	}
	if d.kfWaitTimer != nil {
		d.kfWaitTimer.Stop()
		d.kfWaitTimer = nil
	}
}

// signalWatchdog sends a non-blocking signal to the GfxHandler decodeLoop so
// it calls maybeNotifyDecoderBroken even when no server frames are arriving.
func (d *ffmpegDecoder) signalWatchdog() {
	if d.watchdogCh == nil {
		return
	}
	select {
	case d.watchdogCh <- struct{}{}:
	default:
	}
}

// HardResetCount always returns 0 — hard resets have been removed.
// The method is kept to satisfy the rdpgfx.H264Decoder interface used by GfxHandler.
func (d *ffmpegDecoder) HardResetCount() int {
	return 0
}

func (d *ffmpegDecoder) LastReceiveTime() time.Time {
	return d.lastReceiveTime
}

// setRegionHint specifies dirty rectangles for the next Decode call.  When
// set, convertFrame will use region-aware YUV→BGRA conversion and only write
// pixels within the provided rectangles, skipping unchanged areas of the frame.
// Must be called immediately before Decode; Decode clears the hint at entry so
// it cannot accidentally apply to a later unrelated frame.
func (d *ffmpegDecoder) SetRegionHint(rects [][4]uint16) {
	n := len(rects)
	need := n * 4
	if cap(d.regionHint) < need {
		d.regionHint = make([]C.uint16_t, need)
	} else {
		d.regionHint = d.regionHint[:need]
	}
	for i, r := range rects {
		d.regionHint[i*4+0] = C.uint16_t(r[0])
		d.regionHint[i*4+1] = C.uint16_t(r[1])
		d.regionHint[i*4+2] = C.uint16_t(r[2])
		d.regionHint[i*4+3] = C.uint16_t(r[3])
	}
	d.nRegionHints = C.int(n)
}

func (d *ffmpegDecoder) Decode(h264Data []byte) (*rdpgfx.H264Frame, error) {
	// Capture and clear the pending region hint immediately so that any early
	// return (broken, keyframe wait, etc.) cannot leave stale hints that would
	// incorrectly apply to a subsequent unrelated frame.
	regHint := d.regionHint
	nReg := d.nRegionHints
	d.nRegionHints = 0

	if len(h264Data) == 0 {
		return nil, nil
	}
	if !d.outI420Enabled {
		d.lastI420 = nil
	}
	if !d.outNV12Enabled {
		d.lastNV12 = nil
	}
	if d.broken {
		// HW decoder is unrecoverable.  Stop feeding packets so no frames
		// are produced; the application-level watchdog will reconnect.
		return nil, nil
	}
	// A background timer may have fired and set timerBroken while Decode()
	// was not being called (static screen → server sends no frames).
	// Propagate it to broken so all downstream checks see a consistent state.
	if d.timerBroken.Load() {
		d.markBroken(rdpgfx.H264BrokenReason(d.timerBrokenReason.Load()))
		return nil, nil
	}
	// Track every call, including those that return early (probe mode, keyframe
	// wait, etc.).  Keep the previous receive time for idle detection before we
	// overwrite it with the current call timestamp.
	now := time.Now()
	prevReceiveTime := d.lastReceiveTime
	d.lastReceiveTime = now

	// After a decoder reset we must resync with a fresh IDR from the server.
	// After a SW decoder flush, wait for an IDR before resuming decoding.
	// If the server never sends one within keyframeWaitLimit packets,
	// attempt error-concealment decode anyway.
	// FFmpeg's "[h264 @ ...] sps_id out of range" errors are suppressed at
	// the av_log level (AV_LOG_FATAL) set in newH264Decoder; grdp emits its
	// own slog warning instead.
	// Single pass over the Annex B stream: detect IDR/SPS NAL presence.
	scan := rdpgfx.ScanH264Packet(h264Data)

	if d.needsKeyFrame {
		if !scan.HasKeyFrame {
			d.keyframeWaitCount++
			if d.keyframeWaitCount == 1 {
				d.keyframeWaitStart = time.Now()
				if d.useHW {
					slog.Debug("H.264: HW decoder waiting for IDR")
					// kfWaitTimer was armed at decoder creation; only start a new
					// one here if the decoder was created without a watchdog channel
					// (no timer was armed at creation time).
					if d.kfWaitTimer == nil {
						d.kfWaitTimer = time.AfterFunc(d.kfWaitTimeoutVal, func() {
							d.timerBrokenReason.Store(int32(rdpgfx.H264BrokenReasonNoIDR))
							d.timerBroken.Store(true)
							d.signalWatchdog()
						})
					}
				}
			} else if d.keyframeWaitCount%30 == 0 {
				slog.Debug("H.264: still waiting for IDR",
					"waited", d.keyframeWaitCount,
					"waitedFor", time.Since(d.keyframeWaitStart).Round(time.Millisecond))
			}
			kfTimeout := d.kfWaitTimeoutVal // use per-decoder limit (HW: 15 s, SW fallback: 8 s)
			if !d.useHW && d.watchdogCh == nil {
				// Detached aux SW decoder (h264dec2): shorter wait so it is
				// torn down and recreated quickly on the next stream2 IDR.
				kfTimeout = keyframeWaitTimeoutSW // 5 s
			}
			waitedTooLong := !d.keyframeWaitStart.IsZero() &&
				time.Since(d.keyframeWaitStart) >= kfTimeout
			if d.keyframeWaitCount >= keyframeWaitLimit || waitedTooLong {
				if d.useHW || d.watchdogCh != nil {
					// HW decoders (e.g. VideoToolbox) and watchdog-armed SW
					// decoders (main decoder SW fallback) cannot recover
					// without a proper IDR.  Mark broken so the recovery
					// chain can escalate.  For the SW fallback case, error-
					// concealment on P-frames without reference frames always
					// fails (avcodec_send_packet returns EINVAL) and would
					// only produce a spurious WARN and an immediate reconnect.
					slog.Debug("H.264: no IDR received, marking broken",
						"hw", d.useHW,
						"waited", d.keyframeWaitCount,
						"waitedFor", time.Since(d.keyframeWaitStart).Round(time.Millisecond))
					d.markBroken(rdpgfx.H264BrokenReasonNoIDR)
					return nil, nil
				}
				if d.auxEAGAINFlush {
					// After an EAGAIN-based flush the decoder has no reference
					// frame; error-concealment would immediately fail with EINVAL
					// and trigger markBroken → reconnect.  Instead, keep waiting
					// for the next natural stream2 IDR (Windows Server delivers
					// one at every GOP boundary, typically every 30–60 s).
					// Reset the wait clock so we keep dropping P-frames silently
					// rather than entering the concealment path.
					d.keyframeWaitCount = 0
					d.keyframeWaitStart = time.Now()
					return nil, nil
				}
				slog.Debug("H.264: aux SW decoder: no IDR received, attempting error-concealment",
					"waited", d.keyframeWaitCount,
					"waitedFor", time.Since(d.keyframeWaitStart).Round(time.Millisecond))
				d.needsKeyFrame = false
				d.keyframeWaitCount = 0
				d.keyframeWaitStart = time.Time{}
				d.proceededWithoutKeyframe = true
				// fall through and attempt SW error-concealment decode
			} else {
				if !d.useHW && d.watchdogCh == nil {
					// Aux SW decoder (h264dec2) silently dropping packets while
					// waiting for an IDR.  Log once at first drop so we can
					// distinguish this from the EAGAIN case.
					if d.keyframeWaitCount == 1 {
						slog.Debug("H.264: aux SW decoder lost IDR sync, waiting",
							"h264Len", len(h264Data))
					}
				}
				return nil, nil // drop P-frames while waiting
			}
		} else {
			waitedFor := time.Duration(0)
			if !d.keyframeWaitStart.IsZero() {
				waitedFor = time.Since(d.keyframeWaitStart).Round(time.Millisecond)
			}
			slog.Debug("H.264: IDR received, resuming decode",
				"hw", d.useHW, "waitedFor", waitedFor)
			d.needsKeyFrame = false
			d.keyframeWaitCount = 0
			d.keyframeWaitStart = time.Time{}
			d.auxEAGAINFlush = false // IDR received; EAGAIN recovery complete
			// IDR received — cancel the background wait timer.
			if d.kfWaitTimer != nil {
				d.kfWaitTimer.Stop()
				d.kfWaitTimer = nil
			}
		}
	}

	// If we previously proceeded without a keyframe (error-concealment path)
	// and the server has now sent a proper IDR, the decoder is back to a clean
	// state — clear the flag so a future send failure is not misattributed to
	// the (long-past) keyframe wait exhaustion.
	if d.proceededWithoutKeyframe && scan.HasKeyFrame {
		d.proceededWithoutKeyframe = false
	}

	// VideoToolbox sometimes returns a zero-filled IOSurface on the first
	// frame after any IDR — not only after decoder creation or flush — because
	// the hardware pipeline must drain and reset its reference frames before it
	// can produce the new intra frame.  Re-arm the zero-check whenever we
	// receive an IDR so that convertFrame discards any spurious green frame that
	// VideoToolbox outputs during that transition.
	if d.useHW && scan.HasKeyFrame {
		d.hwNeedsZeroCheck = true
	}

	// Time-based stall detection for the HW decoder.
	//
	// hwReady=false: decoder has never produced a frame. If it keeps receiving
	// packets without ever outputting anything, the VideoToolbox session failed
	// to initialise — mark broken so the soft-reset/reconnect path fires.
	//
	// hwReady=true: decoder was working. VideoToolbox legitimately stalls for
	// several seconds when processing an IDR / scene-change keyframe (it must
	// flush its internal reference pipeline before it can resume output).
	// Firing broken on these stalls causes unnecessary soft-reset loops.  We
	// apply avcHWReadyFreezeThreshold here as a pre-flight guard: if the
	// decoder has been silent for longer than the threshold we mark it broken
	// and return *without* calling avcodec_send_packet.  This is critical
	// because on macOS VideoToolbox the CGo call itself permanently blocks
	// after ~5.75 s of stall, permanently hanging the decodeLoop goroutine.
	//
	// False-positive guard: if the RDP server itself was idle (no packets sent
	// for at least avcHWReadyFreezeThreshold), the elapsed time since
	// lastSuccessTime reflects server silence, not a VideoToolbox deadlock.
	// In that case we reset the stall clock so the threshold applies only to
	// periods where packets were actually flowing into the decoder.
	if d.useHW && !d.hwReady && !d.hwFirstSendTime.IsZero() {
		if stalledFor := time.Since(d.hwFirstSendTime); stalledFor >= avcFreezeThreshold {
			slog.Warn("H.264: HW decoder failed to produce first frame, marking broken",
				"stalledFor", stalledFor, "hwSentCount", d.hwSentCount)
			d.markBroken(rdpgfx.H264BrokenReasonInitFailure)
			return nil, nil
		}
	}
	if d.useHW && d.hwReady && !d.lastSuccessTime.IsZero() {
		// Early probe: the null-frame count detector may have set stallProbeStart
		// before stalledFor reached readyThreshold.  Handle it here so we skip
		// avcodec_send_packet during the probe window even while stalledFor is
		// still below the 7-second CGo-safe threshold.
		if !d.stallProbeStart.IsZero() {
			readyThreshold := avcHWReadyFreezeThreshold
			if d.hwSentCount < avcHWEarlyFrameLimit {
				readyThreshold = avcHWEarlyFreezeThreshold
			}
			if stalledFor := time.Since(d.lastSuccessTime); stalledFor < readyThreshold {
				// Probe active but main threshold not yet crossed.  Try to drain
				// a frame that VT may have buffered; if found the stall was
				// transient and we resume normally.
				if C.avcodec_receive_frame(d.codecCtx, d.frame) >= 0 {
					C.av_frame_unref(d.frame)
					d.lastSuccessTime = time.Now()
					d.hwConsecNullFrames = 0
					d.stallProbeStart = time.Time{}
					if d.stallTimer != nil {
						d.stallTimer.Stop()
						d.stallTimer = nil
					}
					slog.Debug("H.264: HW decoder recovered during early probe (drain found frame)",
						"hwSentCount", d.hwSentCount)
					// Fall through to send the current packet normally.
				} else if probedFor := time.Since(d.stallProbeStart); probedFor >= avcHWRecoveryWindow {
					slog.Debug("H.264: HW decoder early-probe timed out, marking broken",
						"probedFor", probedFor.Round(time.Second),
						"frozenFor", stalledFor.Round(time.Second),
						"hwSentCount", d.hwSentCount)
					d.markBroken(rdpgfx.H264BrokenReasonHWStall)
					return nil, nil
				} else {
					// Still inside probe window: skip send_packet to avoid
					// feeding the stalled VT pipeline.
					return nil, nil
				}
			}
			// else: stalledFor >= readyThreshold — fall through to the
			// threshold-based block below which also handles the probe.
		}
	}
	if d.useHW && d.hwReady && !d.lastSuccessTime.IsZero() {
		readyThreshold := avcHWReadyFreezeThreshold
		if d.hwSentCount < avcHWEarlyFrameLimit {
			readyThreshold = avcHWEarlyFreezeThreshold
		}
		if stalledFor := time.Since(d.lastSuccessTime); stalledFor >= readyThreshold {
			// If no packet had arrived since the previous Decode() call during
			// the apparent stall, the server was simply idle (e.g. screen was
			// static).  Reset the stall clock so we don't misfire on the first
			// packet after a server-side pause.
			if prevReceiveTime.IsZero() || now.Sub(prevReceiveTime) >= readyThreshold {
				slog.Debug("H.264: HW decoder stall clock reset (server was idle)",
					"idleFor", stalledFor, "hwSentCount", d.hwSentCount)
				d.lastSuccessTime = now
				d.stallProbeStart = time.Time{}
				if d.stallTimer != nil {
					d.stallTimer.Stop()
					d.stallTimer = nil
				}
			} else {
				// Probe for pending output that VideoToolbox may be about to
				// produce.  VT legitimately stalls for several seconds at a
				// GOP/IDR boundary while it flushes its reference pipeline;
				// immediately marking broken would cause an unnecessary
				// soft-reset loop followed by a ForceRefresh that the server
				// may not honour with a timely IDR.
				//
				// avcodec_receive_frame is non-blocking and safe to call
				// without a preceding send_packet.  If a frame emerges VT was
				// just slow but is still healthy — reset the stall clock and
				// let the current packet be sent normally below.
				if C.avcodec_receive_frame(d.codecCtx, d.frame) >= 0 {
					C.av_frame_unref(d.frame)
					d.lastSuccessTime = time.Now()
					d.stallProbeStart = time.Time{}
					// Stall resolved — cancel the background probe timer.
					if d.stallTimer != nil {
						d.stallTimer.Stop()
						d.stallTimer = nil
					}
					slog.Debug("H.264: HW decoder stall clock reset (drain found pending frame)",
						"hadBeenSilentFor", stalledFor, "hwSentCount", d.hwSentCount)
					// Fall through to send the current packet normally.
				} else {
					// No output yet.  Enter / stay in recovery-probe window.
					if d.stallProbeStart.IsZero() {
						d.stallProbeStart = now
						slog.Warn("H.264: HW decoder stall detected, probing for recovery",
							"frozenFor", stalledFor.Round(time.Millisecond),
							"hwSentCount", d.hwSentCount)
						// Start a background timer so the probe window expires
						// even when the server sends no more frames.
						d.stallTimer = time.AfterFunc(avcHWRecoveryWindow, func() {
							d.timerBrokenReason.Store(int32(rdpgfx.H264BrokenReasonHWStall))
							d.timerBroken.Store(true)
							d.signalWatchdog()
						})
					} else if probedFor := time.Since(d.stallProbeStart); probedFor >= avcHWRecoveryWindow {
						slog.Debug("H.264: HW decoder recovery probe timed out, marking broken",
							"totalFrozen", stalledFor.Round(time.Second),
							"probedFor", probedFor.Round(time.Second),
							"hwSentCount", d.hwSentCount)
						d.markBroken(rdpgfx.H264BrokenReasonHWStall)
						return nil, nil
					}
					// Still in recovery window: skip send_packet to avoid the
					// ~5.75 s CGo deadlock and wait for VT to resume.
					return nil, nil
				}
			}
		} else if !d.stallProbeStart.IsZero() {
			// Stall resolved (lastSuccessTime updated by normal frame output).
			slog.Debug("H.264: HW decoder recovered from stall",
				"probedFor", time.Since(d.stallProbeStart).Round(time.Millisecond))
			d.stallProbeStart = time.Time{}
			// Cancel the background probe timer — VT recovered on its own.
			if d.stallTimer != nil {
				d.stallTimer.Stop()
				d.stallTimer = nil
			}
		}
	}

	// Pass the Go slice's backing array directly to avcodec_send_packet
	// instead of allocating + copying via C.CBytes for every packet.
	// FFmpeg copies the buffer internally for non-refcounted packets, so the
	// memory only needs to remain valid for the duration of the C call —
	// runtime.KeepAlive guarantees this.
	d.packet.data = (*C.uint8_t)(unsafe.Pointer(&h264Data[0]))
	d.packet.size = C.int(len(h264Data))

	// Count packets sent to HW decoder (for init timeout tracking).
	if d.useHW {
		d.hwSentCount++
		if d.hwSentCount == 1 {
			d.hwFirstSendTime = time.Now()
		}
		d.lastSendTime = time.Now()
	}

	sendStart := time.Now()
	ret := C.avcodec_send_packet(d.codecCtx, d.packet)
	sendNs := time.Since(sendStart).Nanoseconds()
	// Make sure the Go-managed h264Data backing array is not collected or
	// moved while FFmpeg is reading from it inside the C call above.
	runtime.KeepAlive(h264Data)
	// Drop the Go pointer from the AVPacket immediately so a subsequent
	// avcodec_* call can't dereference stale memory.
	d.packet.data = nil
	d.packet.size = 0
	if ret < 0 {
		// Both HW and SW: flush the decoder pipeline and wait for a fresh IDR.
		// Reset the HW stall-timer so it starts fresh after the IDR arrives,
		// not from before this failed send attempt.
		if !d.useHW && d.watchdogCh == nil {
			// Aux SW decoder (h264dec2): a send failure is unexpected and
			// causes all subsequent LC=2 frames to be buffered until the next
			// stream2 IDR.  Upgrade to WARN so it's visible without DEBUG logging.
			slog.Warn("H.264: aux SW decoder send failed, waiting for stream2 IDR",
				"err", int(ret))
		} else {
			slog.Debug("H.264: avcodec_send_packet failed, flushing decoder to recover",
				"hw", d.useHW, "err", int(ret))
		}
		C.avcodec_flush_buffers(d.codecCtx)
		prev := d.proceededWithoutKeyframe
		d.needsKeyFrame = true
		d.keyframeWaitCount = 0
		d.keyframeWaitStart = time.Time{}
		d.proceededWithoutKeyframe = false
		if d.useHW {
			d.hwFirstSendTime = time.Time{} // restart stall clock after IDR
			d.hwSentCount = 0
			d.hwConsecNullFrames = 0
			d.hwNeedsZeroCheck = true // re-check for zero-filled IOSurface after flush
			if !d.hwReady && prev {
				// We gave up waiting for an IDR and tried a P-frame anyway, and
				// VideoToolbox rejected it.  There is no further recovery possible
				// for this decoder context — mark broken so the soft-reset /
				// reconnect chain can proceed.
				slog.Warn("H.264: HW decoder rejected packet after keyframe wait exhaustion, marking broken",
					"err", int(ret))
				d.markBroken(rdpgfx.H264BrokenReasonNoIDR)
			}
		} else if prev {
			// SW decoder: error-concealment attempt (proceededWithoutKeyframe)
			// failed — avcodec_send_packet rejected the P-frame.  Without
			// marking broken the decoder would loop: wait 900 frames → try →
			// fail → reset → wait 900 frames → ...  Mark broken so the
			// soft-reset / reconnect chain can escalate instead.
			slog.Warn("H.264: SW decoder rejected packet after keyframe wait exhaustion, marking broken",
				"err", int(ret))
			d.markBroken(rdpgfx.H264BrokenReasonNoIDR)
		}
		return nil, nil
	}

	// Receive decoded frame(s); keep the last one.
	var result *rdpgfx.H264Frame
	var recvNs, convertNs, transferNs, maxConvNs int64
	for {
		recvStart := time.Now()
		ret = C.avcodec_receive_frame(d.codecCtx, d.frame)
		recvNs += time.Since(recvStart).Nanoseconds()
		if ret < 0 {
			break // EAGAIN (need more input) or EOF
		}
		convStart := time.Now()
		f, tNs, err := d.convertFrame(regHint, nReg)
		dur := time.Since(convStart).Nanoseconds()
		convertNs += dur
		transferNs += tNs
		if dur > maxConvNs {
			maxConvNs = dur
		}
		C.av_frame_unref(d.frame)
		if err != nil {
			return nil, err
		}
		result = f
	}

	// I420/NV12 fast paths return nil for the BGRA frame but still represent a
	// successfully decoded frame.  Count them as success for health tracking.
	gotFrame := result != nil || d.lastI420 != nil || d.lastNV12 != nil
	if gotFrame {
		d.lastSuccessTime = time.Now()
		d.hwConsecNullFrames = 0
		if !d.useHW && d.watchdogCh == nil {
			d.auxEAGAINCount = 0 // successful decode; reset stuck-EAGAIN counter
		}
		if d.useHW {
			if !d.hwReady {
				slog.Debug("H.264: HW decoder produced first frame",
					"hwSentCount", d.hwSentCount)
			}
			d.hwReady = true

			// Aggregate per-frame timing for the HW path.
			d.profFrames++
			d.profSendNs += sendNs
			d.profRecvNs += recvNs
			d.profConvertNs += convertNs
			d.profTransferNs += transferNs
			if maxConvNs > d.profMaxConvNs {
				d.profMaxConvNs = maxConvNs
			}
			if sendNs > d.profMaxSendNs {
				d.profMaxSendNs = sendNs
			}
			if recvNs > d.profMaxRecvNs {
				d.profMaxRecvNs = recvNs
			}
			if d.profFrames >= profileWindow {
				n := int64(d.profFrames)
				slog.Debug("H.264: HW decode timing",
					"frames", d.profFrames,
					"avgSendUs", d.profSendNs/n/1000,
					"avgRecvUs", d.profRecvNs/n/1000,
					"avgConvertUs", d.profConvertNs/n/1000,
					"avgTransferUs", d.profTransferNs/n/1000,
					"maxSendUs", d.profMaxSendNs/1000,
					"maxRecvUs", d.profMaxRecvNs/1000,
					"maxConvertUs", d.profMaxConvNs/1000)
				d.profFrames = 0
				d.profSendNs = 0
				d.profRecvNs = 0
				d.profConvertNs = 0
				d.profTransferNs = 0
				d.profMaxConvNs = 0
				d.profMaxSendNs = 0
				d.profMaxRecvNs = 0
			}
		}
	} else { // !gotFrame
		if !d.useHW && d.watchdogCh == nil {
			// Aux SW decoder (h264dec2) returned no output.  Log for diagnosis.
			slog.Debug("H.264: aux SW decoder produced no output",
				"h264Len", len(h264Data),
				"isIDR", scan.HasKeyFrame,
				"needsKeyFrame", d.needsKeyFrame,
				"recvRet", int(ret))
			d.auxEAGAINCount++
			if d.auxEAGAINCount >= auxEAGAINThreshold {
				// h264dec2 is stuck: avcodec_receive_frame permanently returns
				// EAGAIN despite successful avcodec_send_packet.  This occurs
				// after ~26 decoded frames due to an internal FFmpeg H.264 SW
				// decoder DPB/reorder state that can't self-resolve.
				//
				// Flush the decoder and wait for the next natural stream2 IDR
				// (Windows Server sends IDRs at every GOP boundary, typically
				// every 30–60 s).  This avoids triggering the auxDecoderBroken
				// timer and therefore avoids a session reconnect.
				slog.Warn("H.264: aux SW decoder stuck in EAGAIN, flushing and waiting for stream2 IDR",
					"eagainCount", d.auxEAGAINCount)
				C.avcodec_flush_buffers(d.codecCtx)
				d.needsKeyFrame = true
				d.keyframeWaitCount = 0
				d.keyframeWaitStart = time.Time{}
				d.auxEAGAINCount = 0
				d.auxEAGAINFlush = true // suppress error-concealment until stream2 IDR
			}
		}
		if d.useHW && d.hwReady {
			stalledFor := time.Since(d.lastSuccessTime)
			d.hwConsecNullFrames++
			slog.Debug("H.264: HW null frame", "frozenFor", stalledFor,
				"hwSentCount", d.hwSentCount)
			// Stall probe: trigger a probe if we accumulate many consecutive
			// null frames before the 7-second CGo-safe threshold.  Two tiers:
			//   • Early window (hwSentCount < avcHWEarlyFrameLimit): use
			//     avcHWNullFrameStallLimit (25).  Reduces visible freeze from
			//     ~10 s to ~4 s for a genuine VT stall at session start.
			//   • Mid-session (hwSentCount >= avcHWEarlyFrameLimit): use
			//     avcHWMidSessionNullFrameLimit (150 ≈ 5 s at 30 fps).
			//     Normal GOP boundaries produce ≤25 null frames so there is
			//     comfortable headroom before the false-positive risk zone.
			//     Reduces visible freeze from 7 s to ~5.5 s mid-session.
			earlyStall := d.hwSentCount < avcHWEarlyFrameLimit &&
				d.hwConsecNullFrames >= avcHWNullFrameStallLimit
			midStall := d.hwSentCount >= avcHWEarlyFrameLimit &&
				d.hwConsecNullFrames >= avcHWMidSessionNullFrameLimit
			if (earlyStall || midStall) && d.stallProbeStart.IsZero() {
				slog.Warn("H.264: HW decoder stall detected (null frame count), entering probe",
					"consecNullFrames", d.hwConsecNullFrames,
					"frozenFor", stalledFor.Round(time.Millisecond),
					"hwSentCount", d.hwSentCount)
				d.stallProbeStart = time.Now()
				d.stallTimer = time.AfterFunc(avcHWRecoveryWindow, func() {
					d.timerBrokenReason.Store(int32(rdpgfx.H264BrokenReasonHWStall))
					d.timerBroken.Store(true)
					d.signalWatchdog()
				})
			}
			// Safety valve: if the pre-flight probe window is NOT active and
			// the decoder has been silent past the threshold, VideoToolbox is
			// genuinely stuck.  In probe mode the pre-flight block (above) is
			// responsible for declaring the decoder broken — the safety valve
			// must not interfere with the probe window countdown.
			if d.stallProbeStart.IsZero() && stalledFor >= avcHWReadyFreezeThreshold {
				slog.Warn("H.264: HW decoder stall timeout (safety valve), marking broken",
					"frozenFor", stalledFor, "hwSentCount", d.hwSentCount)
				d.markBroken(rdpgfx.H264BrokenReasonHWStall)
			}
		}
	}
	return result, nil
}

// DecodeWithI420 implements the rdpgfx.I420Decoder interface.  It decodes H.264 NAL
// data and returns both a BGRA frame (for the surface backing store) and an
// optional I420 frame for GPU-accelerated rendering via SDL2 IYUV textures.
// The I420 frame is nil when the decoder's pixel format is not directly
// supported (e.g. swscale paths that have already consumed the source frame
// before we could extract planar data, or hardware-decoded frames whose
// transfer format is not YUV420P or NV12).  Callers must fall back to BGRA
// rendering when I420 is nil.
func (d *ffmpegDecoder) DecodeWithI420(h264Data []byte) (*rdpgfx.H264Frame, *rdpgfx.H264FrameI420, error) {
	d.outI420Enabled = true
	d.lastI420 = nil
	frame, err := d.Decode(h264Data)
	d.outI420Enabled = false
	return frame, d.lastI420, err
}

// DecodeWithNV12 implements the rdpgfx.NV12Decoder interface.  It decodes H.264 NAL
// data and returns native NV12 output when FFmpeg produces NV12, avoiding the
// extra NV12->I420 deinterleave used by DecodeWithI420.
func (d *ffmpegDecoder) DecodeWithNV12(h264Data []byte) (*rdpgfx.H264Frame, *rdpgfx.H264FrameNV12, error) {
	d.outNV12Enabled = true
	d.lastNV12 = nil
	frame, err := d.Decode(h264Data)
	d.outNV12Enabled = false
	return frame, d.lastNV12, err
}

func (d *ffmpegDecoder) convertFrame(regionHint []C.uint16_t, nRegions C.int) (*rdpgfx.H264Frame, int64, error) {
	srcFrame := d.frame
	var transferNs int64
	usedMapFrame := false

	// Transfer from GPU to CPU memory if using hardware acceleration.
	if d.useHW && d.frame.format == C.int(d.hwPixFmt) {
		tStart := time.Now()
		// Prefer zero-copy CPU mapping (av_hwframe_map) over a copy
		// (av_hwframe_transfer_data).  VideoToolbox on macOS stores decoded
		// frames in IOSurface-backed shared memory, so mapping is supported
		// and avoids a full GPU→RAM copy of the pixel data.
		// hwMapSupported: 0=unknown (first frame), 1=ok, -1=unsupported.
		if d.hwMapSupported >= 0 {
			C.av_frame_unref(d.mapFrame)
			if ret := C.grdp_hwframe_map(d.mapFrame, d.frame); ret >= 0 {
				d.hwMapSupported = 1
				srcFrame = d.mapFrame
				usedMapFrame = true
			} else if d.hwMapSupported == 0 {
				// First attempt failed; mark unsupported and fall through.
				d.hwMapSupported = -1
			}
		}
		if !usedMapFrame {
			ret := C.av_hwframe_transfer_data(d.swFrame, d.frame, 0)
			transferNs = time.Since(tStart).Nanoseconds()
			if ret < 0 {
				return nil, transferNs, fmt.Errorf("av_hwframe_transfer_data: error %d", int(ret))
			}
			srcFrame = d.swFrame
		} else {
			transferNs = time.Since(tStart).Nanoseconds()
		}
	}

	w := srcFrame.width
	h := srcFrame.height
	srcFmt := C.enum_AVPixelFormat(srcFrame.format)

	// Fast path for SDL2 NV12 texture upload.  VideoToolbox usually transfers
	// hardware-decoded H.264 frames as NV12, so keeping the interleaved UV plane
	// intact avoids the chroma deinterleave required by I420.
	if d.outNV12Enabled && srcFmt == C.AV_PIX_FMT_NV12 {
		d.extractNV12fromSrc(srcFrame)
		if usedMapFrame {
			C.av_frame_unref(d.mapFrame)
		} else if srcFrame == d.swFrame {
			C.av_frame_unref(d.swFrame)
		}
		return nil, transferNs, nil
	}

	// Fast path: when I420 output is requested and the source pixel format is
	// directly convertible to I420 (YUV420P, YUVJ420P, NV12), skip the
	// YUV→BGRA conversion entirely.  The SDL2 IYUV texture render path does
	// not need BGRA; eliminating the conversion saves roughly w*h*4 bytes of
	// CPU writes per frame (≈8 MB at 1920×1080).
	// Trade-off: blitToSurface will not be called for this frame, so the
	// RDPGFX surface backing store will not reflect the H.264 content.
	// SurfaceToSurface reads from this surface will see stale data, but in
	// practice H.264-decoded surfaces are destination-only in normal sessions.
	if d.outI420Enabled {
		if srcFmt == C.AV_PIX_FMT_YUV420P || srcFmt == C.AV_PIX_FMT_YUVJ420P ||
			srcFmt == C.AV_PIX_FMT_NV12 {
			d.extractI420fromSrc(srcFrame)
			if usedMapFrame {
				C.av_frame_unref(d.mapFrame)
			} else if srcFrame == d.swFrame {
				C.av_frame_unref(d.swFrame)
			}
			return nil, transferNs, nil
		}
	}

	outSize := int(w) * int(h) * 4
	// Borrow the next ring buffer instead of allocating fresh.  At 1920×1080
	// this avoids an 8MB allocation every frame.
	out := d.outRing[d.outRingIdx]
	if cap(out) < outSize {
		out = make([]byte, outSize)
	} else {
		out = out[:outSize]
	}
	d.outRing[d.outRingIdx] = out
	d.outRingIdx ^= 1

	// For planar YUV420P (both limited- and full-range variants), use our own
	// BT.601 conversion instead of swscale on ARM64.  swscale has no
	// accelerated colorspace-conversion path for yuv420p→bgra on ARM64 and
	// its non-accelerated fallback ignores sws_setColorspaceDetails,
	// producing a strong green cast.  On x86_64 swscale is both correct and
	// significantly faster (SIMD-accelerated), so we route through swscale
	// there and only fall back to the hand-written loop on ARM64.
	//
	// For NV12 (VideoToolbox HW transfer output) on ARM64, bypass swscale for
	// the same reason: the non-accelerated ARM64 path ignores
	// sws_setColorspaceDetails and produces a green cast on zero-filled frames.
	var convErr error
	switch {
	case (srcFmt == C.AV_PIX_FMT_YUV420P || srcFmt == C.AV_PIX_FMT_YUVJ420P) && !useSwscale:
		fullRange := C.int(0)
		if srcFmt == C.AV_PIX_FMT_YUVJ420P || srcFrame.color_range == 2 {
			fullRange = 1
		}
		// Log the centre-pixel YUV values for the first few frames so we
		// can distinguish H.264 decode corruption from colour-conversion bugs.
		if d.hwFrameCount < 3 || (!d.useHW && d.swFrameCount < 3) {
			var sy, su, sv C.uint8_t
			C.grdp_sample_yuv(srcFrame, &sy, &su, &sv)
			slog.Debug("H.264: frame sample (yuv420p)",
				"hw", d.useHW,
				"frame", d.hwFrameCount,
				"fmt", int(srcFmt),
				"colorRange", int(srcFrame.color_range),
				"fullRange", int(fullRange),
				"Y", int(sy), "U", int(su), "V", int(sv),
				"w", int(w), "h", int(h))
			if d.useHW {
				d.hwFrameCount++
			} else {
				d.swFrameCount++
			}
		}
		if nRegions > 0 && len(regionHint) > 0 {
			C.grdp_yuv420p_to_bgra_regions(srcFrame,
				(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4), fullRange,
				(*C.uint16_t)(unsafe.Pointer(&regionHint[0])), nRegions)
		} else {
			C.grdp_yuv420p_to_bgra(srcFrame,
				(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4), fullRange)
		}

	case srcFmt == C.AV_PIX_FMT_NV12 && !useSwscale:
		fullRange := C.int(0)
		if srcFrame.color_range == 2 { // AVCOL_RANGE_JPEG
			fullRange = 1
		}
		frameIdx := d.hwFrameCount
		logThis := d.hwFrameCount < 3
		needZeroCheck := d.useHW && d.hwNeedsZeroCheck
		var sy, su, sv C.uint8_t
		if d.hwFrameCount < 3 || needZeroCheck {
			C.grdp_sample_nv12(srcFrame, &sy, &su, &sv)
			if d.hwFrameCount < 3 {
				slog.Debug("H.264: frame sample (nv12)",
					"hw", d.useHW,
					"frame", d.hwFrameCount,
					"fmt", int(srcFmt),
					"colorRange", int(srcFrame.color_range),
					"fullRange", int(fullRange),
					"Y", int(sy), "U", int(su), "V", int(sv),
					"w", int(w), "h", int(h))
				d.hwFrameCount++
			}
		}
		// Zero-filled IOSurface detection: VideoToolbox sometimes returns an
		// uninitialised (all-zero) IOSurface on the first decoded frame after
		// decoder init or avcodec_flush_buffers.  BT.601 limited-range
		// conversion of (Y=0, U=0, V=0) yields BGRA(0,135,0,255), a
		// full-screen dark-green frame.  Valid NV12 chroma always centres on
		// 128, so U=0 and V=0 simultaneously at the centre pixel is an
		// unambiguous indicator of an uninitialised buffer.  Drop the frame
		// and keep hwNeedsZeroCheck set so we continue checking until a frame
		// with valid chroma arrives.
		if needZeroCheck {
			if su == 0 && sv == 0 {
				slog.Debug("H.264: dropping zero-filled HW frame (IOSurface not ready)",
					"Y", int(sy))
				if usedMapFrame {
					C.av_frame_unref(d.mapFrame)
				} else if srcFrame == d.swFrame {
					C.av_frame_unref(d.swFrame)
				}
				// Return a non-nil Dropped frame so Decode() counts this as a
				// successful VideoToolbox output (health tracking stays correct)
				// and callers skip the keyframe-request / decoder-broken path.
				return &rdpgfx.H264Frame{Dropped: true, Width: int(w), Height: int(h)}, transferNs, nil
			}
			// Valid chroma seen — IOSurface is properly populated.
			d.hwNeedsZeroCheck = false
		}
		if nRegions > 0 && len(regionHint) > 0 {
			C.grdp_nv12_to_bgra_regions(srcFrame,
				(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4), fullRange,
				(*C.uint16_t)(unsafe.Pointer(&regionHint[0])), nRegions)
		} else {
			C.grdp_nv12_to_bgra(srcFrame,
				(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4), fullRange)
		}
		if logThis {
			// Sample NV12 input and BGRA output at multiple positions for
			// the first three frames to diagnose colour conversion.
			for _, p := range [][2]int{{100, 50}, {500, 50}, {960, 50}, {1400, 50}, {960, 200}} {
				px, py := p[0], p[1]
				if px >= int(w) || py >= int(h) {
					continue
				}
				var sy, su, sv C.uint8_t
				C.grdp_sample_nv12_at(srcFrame, C.int(px), C.int(py), &sy, &su, &sv)
				off := (py*int(w) + px) * 4
				slog.Debug("H.264: pixel sample (nv12→bgra)",
					"frame", frameIdx,
					"hw", d.useHW,
					"x", px, "y", py,
					"Y", int(sy), "U", int(su), "V", int(sv),
					"B", out[off], "G", out[off+1], "R", out[off+2])
			}
		}

	default:
		// For other formats, use swscale.
		swsFmt := C.grdp_yuvj_to_yuv(srcFmt)
		fullRange := C.grdp_is_full_range_fmt(srcFmt)
		if fullRange == 0 && srcFrame.color_range == 2 { // AVCOL_RANGE_JPEG
			fullRange = 1
		}
		if d.hwFrameCount < 3 {
			slog.Debug("H.264: frame sample (swscale)",
				"hw", d.useHW,
				"frame", d.hwFrameCount,
				"fmt", int(srcFmt),
				"colorRange", int(srcFrame.color_range),
				"fullRange", int(fullRange),
				"w", int(w), "h", int(h))
			d.hwFrameCount++
		}
		if w != d.lastW || h != d.lastH || srcFmt != d.lastFmt || fullRange != d.lastFullRange {
			if d.swsCtx != nil {
				C.sws_freeContext(d.swsCtx)
			}
			d.swsCtx = C.sws_getContext(
				w, h, swsFmt,
				w, h, C.AV_PIX_FMT_BGRA,
				C.SWS_FAST_BILINEAR, nil, nil, nil,
			)
			if d.swsCtx == nil {
				convErr = fmt.Errorf("sws_getContext failed for %dx%d fmt=%d", w, h, srcFmt)
				break
			}
			C.grdp_sws_set_src_range(d.swsCtx, fullRange)
			d.lastW = w
			d.lastH = h
			d.lastFmt = srcFmt
			d.lastFullRange = fullRange
		}
		C.grdp_frame_to_bgra(d.swsCtx, srcFrame,
			(*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(w*4))
	}

	if convErr == nil && d.outI420Enabled {
		d.extractI420fromSrc(srcFrame)
	}
	if usedMapFrame {
		C.av_frame_unref(d.mapFrame)
	} else if srcFrame == d.swFrame {
		C.av_frame_unref(d.swFrame)
	}
	if convErr != nil {
		return nil, transferNs, convErr
	}
	return &rdpgfx.H264Frame{Data: out, Width: int(w), Height: int(h)}, transferNs, nil
}

func (d *ffmpegDecoder) Close() {
	// Stop any background timers so their callbacks don't fire after Close.
	d.stopTimers()
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
	if d.mapFrame != nil {
		C.av_frame_free(&d.mapFrame)
	}
	if d.packet != nil {
		C.av_packet_free(&d.packet)
	}
	if d.codecCtx != nil {
		C.avcodec_free_context(&d.codecCtx)
	}
}

func init() {
	rdpgfx.SetH264Backend(&rdpgfx.H264DecoderBackend{
		NewHW: func(ch chan<- struct{}) rdpgfx.H264Decoder {
			return newH264DecoderInternal(ch, false, keyframeWaitTimeout)
		},
		NewSW: func() rdpgfx.H264Decoder {
			return newH264DecoderInternal(nil, true, keyframeWaitTimeoutSW)
		},
		NewSWFallback: func(ch chan<- struct{}) rdpgfx.H264Decoder {
			return newH264DecoderInternal(ch, true, keyframeWaitTimeoutSWFallback)
		},
	})
}
