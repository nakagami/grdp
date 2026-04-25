//go:build !amd64 && !arm64

package grdp

// bgr32BatchToRGBA converts n BGRA32 pixels (4 bytes each: B,G,R,X) to RGBA.
func bgr32BatchToRGBA(dst []byte, src []byte, n int) {
	for i := 0; i < n; i++ {
		s := i * 4
		dst[i*4] = src[s+2]
		dst[i*4+1] = src[s+1]
		dst[i*4+2] = src[s]
		dst[i*4+3] = 0xFF
	}
}

// rgb555BatchToRGBA converts n big-endian RGB555 pixels (src, 2 bytes each)
// to RGBA (dst, 4 bytes each). n must be valid for the slice sizes.
func rgb555BatchToRGBA(dst []byte, src []byte, n int) {
	for i := 0; i < n; i++ {
		d := uint16(src[i*2])<<8 | uint16(src[i*2+1])
		dst[i*4] = uint8((d & 0x7C00) >> 7)
		dst[i*4+1] = uint8((d & 0x03E0) >> 2)
		dst[i*4+2] = uint8((d & 0x001F) << 3)
		dst[i*4+3] = 0xFF
	}
}

// rgb565BatchToRGBA converts n big-endian RGB565 pixels to RGBA.
func rgb565BatchToRGBA(dst []byte, src []byte, n int) {
	for i := 0; i < n; i++ {
		d := uint16(src[i*2])<<8 | uint16(src[i*2+1])
		dst[i*4] = uint8((d & 0xF800) >> 8)
		dst[i*4+1] = uint8((d & 0x07E0) >> 3)
		dst[i*4+2] = uint8((d & 0x001F) << 3)
		dst[i*4+3] = 0xFF
	}
}
