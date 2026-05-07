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
static void grdp_copy_yuv420p_to_i420(
    const AVFrame *f,
    uint8_t *ydst, uint8_t *udst, uint8_t *vdst,
    int w, int h)
{
    int pw = (w + 1) / 2;
    int ph = (h + 1) / 2;
    for (int y = 0; y < h; y++)
        memcpy(ydst + y * w, f->data[0] + y * f->linesize[0], w);
    for (int y = 0; y < ph; y++)
        memcpy(udst + y * pw, f->data[1] + y * f->linesize[1], pw);
    for (int y = 0; y < ph; y++)
        memcpy(vdst + y * pw, f->data[2] + y * f->linesize[2], pw);
}

// grdp_copy_nv12_to_i420 copies an AVFrame in NV12 format (Y plane + interleaved UV)
// to tightly-packed I420 planes.
static void grdp_copy_nv12_to_i420(
    const AVFrame *f,
    uint8_t *ydst, uint8_t *udst, uint8_t *vdst,
    int w, int h)
{
    int pw = (w + 1) / 2;
    int ph = (h + 1) / 2;
    for (int y = 0; y < h; y++)
        memcpy(ydst + y * w, f->data[0] + y * f->linesize[0], w);
    for (int y = 0; y < ph; y++) {
        const uint8_t *row = f->data[1] + y * f->linesize[1];
        for (int x = 0; x < pw; x++) {
            udst[y * pw + x] = row[x * 2];
            vdst[y * pw + x] = row[x * 2 + 1];
        }
    }
}
*/
import "C"

import (
	"bytes"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
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

// avcHWRecoveryWindow is how long Decode() probes for pending output after
// avcHWReadyFreezeThreshold is crossed.  VideoToolbox may legitimately stall
// for 1-2 s at a GOP/IDR boundary while flushing its reference pipeline; a
// 3 s probe window (added on top of the 7 s threshold) gives it up to 10 s
// total before we declare the decoder broken and trigger a soft reset.
// Keeping this window short minimises the visual freeze on genuine failures
// since YouTube / gnome-remote-desktop sends a fresh IDR within 2 s of the
// soft reset (either via ForceRefresh or a natural GOP boundary).
const avcHWRecoveryWindow = 3 * time.Second

// keyframeWaitLimit is the maximum number of non-IDR packets we drop while
// waiting for a keyframe after a decoder reset or flush.  gnome-remote-desktop
// and similar servers send an IDR approximately every 15-25 seconds; using 900
// frames (~30s at 30 fps) ensures we catch the next natural IDR even under
// variable server GOP intervals.  After this limit the SW decoder attempts
// error-concealment decode; the HW decoder marks itself broken instead (HW
// codecs like VideoToolbox cannot recover without a proper IDR).
const keyframeWaitLimit = 900

// keyframeWaitTimeout is the maximum wall-clock time the HW decoder waits for
// an IDR after entering needsKeyFrame=true.  ForceRefresh is sent every 2 s,
// so the server should respond within a few seconds.  If no IDR arrives within
// this window the decoder marks itself broken so the soft-reset / reconnect
// chain escalates quickly rather than waiting the full keyframeWaitLimit
// (~30 s) of dropped packets.
const keyframeWaitTimeout = 15 * time.Second

// profileWindow is the number of HW frames over which Decode aggregates
// timing measurements before logging an INFO summary.  At 30 fps this is
// roughly one log line every ~10 s.
const profileWindow = 300

type ffmpegDecoder struct {
	codecCtx                 *C.AVCodecContext
	packet                   *C.AVPacket
	frame                    *C.AVFrame
	swFrame                  *C.AVFrame
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
	brokenReason             h264BrokenReason
	timerBroken              atomic.Bool // set by background timers when probe/IDR timeouts expire
	timerBrokenReason        atomic.Int32
	proceededWithoutKeyframe bool            // "proceed without keyframe" path was taken; AVERROR here means broken
	stallProbeStart          time.Time       // wall-clock time we entered the stall recovery-probe window
	stallTimer               *time.Timer     // fires after avcHWRecoveryWindow to mark broken independently of frame rate
	kfWaitTimer              *time.Timer     // fires after keyframeWaitTimeout to mark broken independently of frame rate
	watchdogCh               chan<- struct{} // signals GfxHandler.decodeLoop to call maybeNotifyDecoderBroken

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
	outI420Ring    [2]h264FrameI420
	outI420RingIdx int
	outI420Enabled bool
	lastI420       *h264FrameI420

	// regionHint carries dirty-rect hints for region-aware YUV→BGRA conversion.
	// setRegionHint populates these fields; Decode() captures them into local
	// variables at entry (clearing nRegionHints) so stale hints can never
	// carry over to a subsequent unrelated frame.
	regionHint  []C.uint16_t // flat [left,top,right,bottom,...] per rect
	nRegionHints C.int        // number of valid rects in regionHint
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

func newH264Decoder() h264Decoder { return newH264DecoderWithWatchdog(nil) }

func newH264DecoderWithWatchdog(watchdogCh chan<- struct{}) h264Decoder {
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
		codecCtx:      codecCtx,
		hwPixFmt:      C.AV_PIX_FMT_NONE,
		lastFmt:       C.AV_PIX_FMT_NONE,
		needsKeyFrame: true, // always wait for a clean IDR before feeding packets
		watchdogCh:    watchdogCh,
	}

	// Always enable LOW_DELAY: RDP H.264 streams are transmitted in display
	// order with no B-frame reordering, so the default reorder buffer adds
	// no value and (especially on VideoToolbox) makes the decoder appear
	// stalled between IDRs.
	C.grdp_set_low_delay(codecCtx)

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
		// Limit the decoded picture buffer to 1 reference frame so each frame
		// is emitted immediately rather than waiting for up to
		// max_dec_frame_buffering (often 8) frames to accumulate.  RDP H.264
		// streams use sequential P-frames that only reference the immediately
		// preceding frame, so this is safe.  VideoToolbox (HW path) has its
		// own zero-latency output mechanism and does not need this.
		codecCtx.refs = 1
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

func (d *ffmpegDecoder) NeedsIDR() bool {
	return d.needsKeyFrame
}

func (d *ffmpegDecoder) IsBroken() bool {
	return d.broken || d.timerBroken.Load()
}

func (d *ffmpegDecoder) BrokenReason() h264BrokenReason {
	if d.brokenReason != h264BrokenReasonNone {
		return d.brokenReason
	}
	return h264BrokenReason(d.timerBrokenReason.Load())
}

func (d *ffmpegDecoder) ForceBroken(reason h264BrokenReason) {
	d.markBroken(reason)
}

// markBroken sets d.broken and stops any pending background timers.
// Called from inside Decode() (decodeLoop goroutine) when a timeout fires.
func (d *ffmpegDecoder) markBroken(reason h264BrokenReason) {
	d.broken = true
	if reason != h264BrokenReasonNone {
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
// The method is kept to satisfy the h264Decoder interface used by GfxHandler.
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
func (d *ffmpegDecoder) setRegionHint(regions []avcRect) {
	n := len(regions)
	need := n * 4
	if cap(d.regionHint) < need {
		d.regionHint = make([]C.uint16_t, need)
	} else {
		d.regionHint = d.regionHint[:need]
	}
	for i, r := range regions {
		d.regionHint[i*4+0] = C.uint16_t(r.left)
		d.regionHint[i*4+1] = C.uint16_t(r.top)
		d.regionHint[i*4+2] = C.uint16_t(r.right)
		d.regionHint[i*4+3] = C.uint16_t(r.bottom)
	}
	d.nRegionHints = C.int(n)
}

func (d *ffmpegDecoder) Decode(h264Data []byte) (*h264Frame, error) {
	// Capture and clear the pending region hint immediately so that any early
	// return (broken, keyframe wait, etc.) cannot leave stale hints that would
	// incorrectly apply to a subsequent unrelated frame.
	regHint := d.regionHint
	nReg := d.nRegionHints
	d.nRegionHints = 0

	if len(h264Data) == 0 {
		return nil, nil
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
		d.markBroken(h264BrokenReason(d.timerBrokenReason.Load()))
		return nil, nil
	}
	// Track every call, including those that return early (probe mode, keyframe
	// wait, etc.).  Used for server-idle detection: if no packets have arrived
	// for avcHWReadyFreezeThreshold, the server is genuinely quiet, not us.
	d.lastReceiveTime = time.Now()

	// After a decoder reset we must resync with a fresh IDR from the server.
	// After a SW decoder flush, wait for an IDR before resuming decoding.
	// If the server never sends one within keyframeWaitLimit packets,
	// attempt error-concealment decode anyway.
	// FFmpeg's "[h264 @ ...] sps_id out of range" errors are suppressed at
	// the av_log level (AV_LOG_FATAL) set in newH264Decoder; grdp emits its
	// own slog warning instead.
	// Single pass over the Annex B stream: detect IDR/SPS NAL presence.
	scan := scanH264Packet(h264Data)

	if d.needsKeyFrame {
		if !scan.hasKeyFrame {
			d.keyframeWaitCount++
			if d.keyframeWaitCount == 1 {
				d.keyframeWaitStart = time.Now()
				if d.useHW {
					slog.Debug("H.264: HW decoder waiting for IDR")
					// Start a background timer so the timeout fires even when
					// the server sends no frames (static screen → 0 fps).
					d.kfWaitTimer = time.AfterFunc(keyframeWaitTimeout, func() {
						d.timerBrokenReason.Store(int32(h264BrokenReasonNoIDR))
						d.timerBroken.Store(true)
						d.signalWatchdog()
					})
				}
			} else if d.keyframeWaitCount%30 == 0 {
				slog.Debug("H.264: still waiting for IDR",
					"waited", d.keyframeWaitCount,
					"waitedFor", time.Since(d.keyframeWaitStart).Round(time.Millisecond))
			}
			waitedTooLong := d.useHW && !d.keyframeWaitStart.IsZero() &&
				time.Since(d.keyframeWaitStart) >= keyframeWaitTimeout
			if d.keyframeWaitCount >= keyframeWaitLimit || waitedTooLong {
				if d.useHW {
					// HW decoders (e.g. VideoToolbox) cannot recover without a
					// proper IDR.  Mark broken so the recovery chain can
					// escalate to a reconnect rather than looping forever.
					slog.Debug("H.264: HW decoder: no IDR received, marking broken",
						"waited", d.keyframeWaitCount,
						"waitedFor", time.Since(d.keyframeWaitStart).Round(time.Millisecond))
					d.markBroken(h264BrokenReasonNoIDR)
					return nil, nil
				}
				slog.Debug("H.264: no IDR received, proceeding without keyframe",
					"waited", d.keyframeWaitCount)
				d.needsKeyFrame = false
				d.keyframeWaitCount = 0
				d.keyframeWaitStart = time.Time{}
				d.proceededWithoutKeyframe = true
				// fall through and attempt SW error-concealment decode
			} else {
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
			// IDR received — cancel the background wait timer.
			if d.kfWaitTimer != nil {
				d.kfWaitTimer.Stop()
				d.kfWaitTimer = nil
			}
		}
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
			d.markBroken(h264BrokenReasonInitFailure)
			return nil, nil
		}
	}
	if d.useHW && d.hwReady && !d.lastSuccessTime.IsZero() {
		if stalledFor := time.Since(d.lastSuccessTime); stalledFor >= avcHWReadyFreezeThreshold {
			// If no packet has arrived at Decode() during the apparent stall,
			// the server was simply idle (e.g. screen was static).  Reset the
			// stall clock so we don't misfire on the first packet after a
			// server-side pause.  Use lastReceiveTime (updated on every
			// Decode() call, even in probe mode) rather than lastSendTime so
			// that probe-mode early returns don't make us appear idle.
			if d.lastReceiveTime.IsZero() || time.Since(d.lastReceiveTime) >= avcHWReadyFreezeThreshold {
				slog.Debug("H.264: HW decoder stall clock reset (server was idle)",
					"idleFor", stalledFor, "hwSentCount", d.hwSentCount)
				d.lastSuccessTime = time.Now()
				d.stallProbeStart = time.Time{}
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
						d.stallProbeStart = time.Now()
						slog.Debug("H.264: HW decoder stall detected, probing for recovery",
							"frozenFor", stalledFor.Round(time.Millisecond),
							"hwSentCount", d.hwSentCount)
						// Start a background timer so the probe window expires
						// even when the server sends no more frames.
						d.stallTimer = time.AfterFunc(avcHWRecoveryWindow, func() {
							d.timerBrokenReason.Store(int32(h264BrokenReasonHWStall))
							d.timerBroken.Store(true)
							d.signalWatchdog()
						})
					} else if probedFor := time.Since(d.stallProbeStart); probedFor >= avcHWRecoveryWindow {
						slog.Debug("H.264: HW decoder recovery probe timed out, marking broken",
							"totalFrozen", stalledFor.Round(time.Second),
							"probedFor", probedFor.Round(time.Second),
							"hwSentCount", d.hwSentCount)
						d.markBroken(h264BrokenReasonHWStall)
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
		slog.Debug("H.264: avcodec_send_packet failed, flushing decoder to recover",
			"hw", d.useHW, "err", int(ret))
		C.avcodec_flush_buffers(d.codecCtx)
		prev := d.proceededWithoutKeyframe
		d.needsKeyFrame = true
		d.keyframeWaitCount = 0
		d.keyframeWaitStart = time.Time{}
		d.proceededWithoutKeyframe = false
		if d.useHW {
			d.hwFirstSendTime = time.Time{} // restart stall clock after IDR
			d.hwSentCount = 0
			if !d.hwReady && prev {
				// We gave up waiting for an IDR and tried a P-frame anyway, and
				// VideoToolbox rejected it.  There is no further recovery possible
				// for this decoder context — mark broken so the soft-reset /
				// reconnect chain can proceed.
				slog.Warn("H.264: HW decoder rejected packet after keyframe wait exhaustion, marking broken",
					"err", int(ret))
				d.markBroken(h264BrokenReasonNoIDR)
			}
		}
		return nil, nil
	}

	// Receive decoded frame(s); keep the last one.
	var result *h264Frame
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

	if result != nil {
		d.lastSuccessTime = time.Now()
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
	} else {
		if d.useHW && d.hwReady {
			stalledFor := time.Since(d.lastSuccessTime)
			slog.Debug("H.264: HW null frame", "frozenFor", stalledFor,
				"hwSentCount", d.hwSentCount)
			// Safety valve: if the pre-flight probe window is NOT active and
			// the decoder has been silent past the threshold, VideoToolbox is
			// genuinely stuck.  In probe mode the pre-flight block (above) is
			// responsible for declaring the decoder broken — the safety valve
			// must not interfere with the probe window countdown.
			if d.stallProbeStart.IsZero() && stalledFor >= avcHWReadyFreezeThreshold {
				slog.Warn("H.264: HW decoder stall timeout (safety valve), marking broken",
					"frozenFor", stalledFor, "hwSentCount", d.hwSentCount)
				d.markBroken(h264BrokenReasonHWStall)
			}
		}
	}
	return result, nil
}

// DecodeWithI420 implements the i420Decoder interface.  It decodes H.264 NAL
// data and returns both a BGRA frame (for the surface backing store) and an
// optional I420 frame for GPU-accelerated rendering via SDL2 IYUV textures.
// The I420 frame is nil when the decoder's pixel format is not directly
// supported (e.g. swscale paths that have already consumed the source frame
// before we could extract planar data, or hardware-decoded frames whose
// transfer format is not YUV420P or NV12).  Callers must fall back to BGRA
// rendering when I420 is nil.
func (d *ffmpegDecoder) DecodeWithI420(h264Data []byte) (*h264Frame, *h264FrameI420, error) {
	d.outI420Enabled = true
	d.lastI420 = nil
	frame, err := d.Decode(h264Data)
	d.outI420Enabled = false
	return frame, d.lastI420, err
}

func (d *ffmpegDecoder) convertFrame(regionHint []C.uint16_t, nRegions C.int) (*h264Frame, int64, error) {
	srcFrame := d.frame
	var transferNs int64

	// Transfer from GPU to CPU memory if using hardware acceleration.
	if d.useHW && d.frame.format == C.int(d.hwPixFmt) {
		tStart := time.Now()
		ret := C.av_hwframe_transfer_data(d.swFrame, d.frame, 0)
		transferNs = time.Since(tStart).Nanoseconds()
		if ret < 0 {
			return nil, transferNs, fmt.Errorf("av_hwframe_transfer_data: error %d", int(ret))
		}
		srcFrame = d.swFrame
	}

	w := srcFrame.width
	h := srcFrame.height
	srcFmt := C.enum_AVPixelFormat(srcFrame.format)

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
		if d.hwFrameCount < 3 {
			var sy, su, sv C.uint8_t
			C.grdp_sample_nv12(srcFrame, &sy, &su, &sv)
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
				C.SWS_BILINEAR, nil, nil, nil,
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
	if srcFrame == d.swFrame {
		C.av_frame_unref(d.swFrame)
	}
	if convErr != nil {
		return nil, transferNs, convErr
	}
	return &h264Frame{Data: out, Width: int(w), Height: int(h)}, transferNs, nil
}

// scanResult holds the IDR-presence flag and SPS/PPS NAL boundaries (offsets
// into the original packet, including Annex B start code) discovered during a
// single linear walk of an Annex B H.264 packet.  Use scanH264Packet to
// produce one.
type scanResult struct {
	hasKeyFrame                        bool
	spsStart, spsEnd, ppsStart, ppsEnd int
}

// scanH264Packet walks an Annex B H.264 packet exactly once, returning
// whether it contains any IDR slice (NAL type 5) or SPS (NAL type 7) NAL
// unit and recording the byte ranges for the most recent SPS/PPS NALs found
// (start offset includes the Annex B start code).  Replaces what used to be
// three separate linear scans (h264ContainsKeyFrame ×2 + scanAndCacheParamSets)
// performed per-packet.
//
// Uses bytes.Index to locate the canonical 3-byte start code (0x000001),
// promoting it to the 4-byte form when preceded by a zero.  bytes.Index is
// implemented in optimized assembly on the major Go platforms, so this
// out-performs a hand-rolled byte loop especially for the long IDR packets
// that dominate the H.264 hot path.
func scanH264Packet(data []byte) scanResult {
	var r scanResult
	startCode := []byte{0, 0, 1}
	pos := 0
	// nalStart points to the byte just past the previous NAL header byte
	// (i.e. into the NAL payload), used as the lower bound when searching
	// for the *next* start code so we never re-find the current one.
	for pos < len(data) {
		off := bytes.Index(data[pos:], startCode)
		if off < 0 {
			break
		}
		i := pos + off
		scLen := 3
		if i > 0 && data[i-1] == 0 {
			i--
			scLen = 4
		}
		if i+scLen >= len(data) {
			break
		}
		nalType := data[i+scLen] & 0x1F
		if nalType == 5 || nalType == 7 {
			r.hasKeyFrame = true
		}
		if nalType == 7 || nalType == 8 {
			// Locate the end of this NAL: search for the next 0x000001
			// from just past the NAL header byte.
			searchFrom := i + scLen + 1
			j := len(data)
			if searchFrom < len(data) {
				if next := bytes.Index(data[searchFrom:], startCode); next >= 0 {
					j = searchFrom + next
					// If preceded by a zero, that zero belongs to the
					// next start code (4-byte form).
					if j > 0 && data[j-1] == 0 {
						j--
					}
				}
			}
			if nalType == 7 {
				r.spsStart, r.spsEnd = i, j
			} else {
				r.ppsStart, r.ppsEnd = i, j
			}
			pos = j
			continue
		}
		pos = i + scLen + 1
	}
	return r
}

// h264PacketHasIDR reports whether an Annex B H.264 packet contains an IDR
// (keyframe) NAL unit.  Used by AVC444 logic in avc.go (no h264 build tag)
// to gate aux-decoder recreation on stream2 IDR availability.
func h264PacketHasIDR(data []byte) bool {
	return scanH264Packet(data).hasKeyFrame
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
	if d.packet != nil {
		C.av_packet_free(&d.packet)
	}
	if d.codecCtx != nil {
		C.avcodec_free_context(&d.codecCtx)
	}
}
