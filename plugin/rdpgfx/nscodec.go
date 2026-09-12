package rdpgfx

import (
	"encoding/binary"
	"sync"
)

// NSCodec decoder for Remote Desktop Protocol (MS-RDPNSC).
// Also used as subcodec 1 in ClearCodec (MS-RDPEGFX 2.2.4.1.1.4).

var nscPlanePool = sync.Pool{
	New: func() any {
		return make([]byte, 0, 64*64)
	},
}

func acquireNSCBuf(size int) []byte {
	b := nscPlanePool.Get().([]byte)
	if cap(b) < size {
		return make([]byte, size)
	}
	return b[:size]
}

func releaseNSCBuf(b []byte) {
	if b != nil {
		nscPlanePool.Put(b[:0])
	}
}

func decodeNSCodec(data []byte, width, height int, out []byte, xStart, yStart, surfW, surfH int) bool {
	if len(data) < 20 || width <= 0 || height <= 0 {
		return false
	}

	l0 := int(binary.LittleEndian.Uint32(data[0:]))
	l1 := int(binary.LittleEndian.Uint32(data[4:]))
	l2 := int(binary.LittleEndian.Uint32(data[8:]))
	l3 := int(binary.LittleEndian.Uint32(data[12:]))
	colorLossLevel := data[16]
	chromaSubsampling := data[17]
	// data[18:20] = reserved

	if colorLossLevel < 1 || colorLossLevel > 7 {
		colorLossLevel = 1
	}

	rw := (width + 7) &^ 7
	rh := (height + 1) &^ 1

	var orgSizes [4]int
	if chromaSubsampling != 0 {
		orgSizes[0] = rw * height
		orgSizes[1] = (rw / 2) * (rh / 2)
		orgSizes[2] = (rw / 2) * (rh / 2)
		orgSizes[3] = width * height
	} else {
		orgSizes[0] = width * height
		orgSizes[1] = width * height
		orgSizes[2] = width * height
		orgSizes[3] = width * height
	}

	planeLens := [4]int{l0, l1, l2, l3}
	var planes [4][]byte
	defer func() {
		for i := 0; i < 4; i++ {
			releaseNSCBuf(planes[i])
		}
	}()

	off := 20
	for i := 0; i < 4; i++ {
		origSize := orgSizes[i]
		planes[i] = acquireNSCBuf(origSize)
		pLen := planeLens[i]
		if pLen == 0 {
			for j := 0; j < origSize; j++ {
				planes[i][j] = 0xFF
			}
		} else if pLen < origSize {
			if off+pLen > len(data) {
				return false
			}
			if !nscRLEDecode(data[off:off+pLen], planes[i], origSize) {
				return false
			}
			off += pLen
		} else {
			if off+origSize > len(data) {
				return false
			}
			copy(planes[i], data[off:off+origSize])
			off += pLen
		}
	}

	shift := colorLossLevel - 1
	yPlane := planes[0]
	coPlane := planes[1]
	cgPlane := planes[2]
	aPlane := planes[3]

	for y := 0; y < height; y++ {
		dstY := yStart + y
		if dstY < 0 || dstY >= surfH {
			continue
		}

		var yRow []byte
		var coRow, cgRow []byte
		if chromaSubsampling != 0 {
			yRow = yPlane[y*rw : (y+1)*rw]
			coOffset := (y >> 1) * (rw >> 1)
			coRow = coPlane[coOffset : coOffset+(rw>>1)]
			cgRow = cgPlane[coOffset : coOffset+(rw>>1)]
		} else {
			yRow = yPlane[y*width : (y+1)*width]
			coRow = coPlane[y*width : (y+1)*width]
			cgRow = cgPlane[y*width : (y+1)*width]
		}
		aRow := aPlane[y*width : (y+1)*width]

		for x := 0; x < width; x++ {
			dstX := xStart + x
			if dstX < 0 || dstX >= surfW {
				continue
			}

			yVal := int16(yRow[x])
			var coIdx, cgIdx int
			if chromaSubsampling != 0 {
				coIdx = x >> 1
				cgIdx = x >> 1
			} else {
				coIdx = x
				cgIdx = x
			}

			coVal := int16(int8(coRow[coIdx] << shift))
			cgVal := int16(int8(cgRow[cgIdx] << shift))

			rVal := yVal + coVal - cgVal
			gVal := yVal + cgVal
			bVal := yVal - coVal - cgVal

			dstIdx := (dstY*surfW + dstX) * 4
			if dstIdx+4 <= len(out) {
				if bVal < 0 {
					out[dstIdx] = 0
				} else if bVal > 255 {
					out[dstIdx] = 255
				} else {
					out[dstIdx] = byte(bVal)
				}

				if gVal < 0 {
					out[dstIdx+1] = 0
				} else if gVal > 255 {
					out[dstIdx+1] = 255
				} else {
					out[dstIdx+1] = byte(gVal)
				}

				if rVal < 0 {
					out[dstIdx+2] = 0
				} else if rVal > 255 {
					out[dstIdx+2] = 255
				} else {
					out[dstIdx+2] = byte(rVal)
				}

				out[dstIdx+3] = aRow[x]
			}
		}
	}

	return true
}

func nscRLEDecode(in []byte, out []byte, originalSize int) bool {
	left := originalSize
	inPos := 0
	outPos := 0

	for left > 4 {
		if inPos >= len(in) {
			return false
		}
		val := in[inPos]
		inPos++

		if left == 5 {
			if outPos >= len(out) {
				return false
			}
			out[outPos] = val
			outPos++
			left--
		} else if inPos >= len(in) {
			return false
		} else if val == in[inPos] {
			inPos++
			if inPos >= len(in) {
				return false
			}
			var runLen int
			if in[inPos] < 0xFF {
				runLen = int(in[inPos]) + 2
				inPos++
			} else {
				if inPos+5 > len(in) {
					return false
				}
				inPos++ // skip 0xFF
				runLen = int(binary.LittleEndian.Uint32(in[inPos:]))
				inPos += 4
			}
			if outPos+runLen > len(out) || left < runLen {
				return false
			}
			for r := 0; r < runLen; r++ {
				out[outPos+r] = val
			}
			outPos += runLen
			left -= runLen
		} else {
			if outPos >= len(out) {
				return false
			}
			out[outPos] = val
			outPos++
			left--
		}
	}

	if outPos+4 > len(out) || left < 4 || inPos+4 > len(in) {
		return false
	}
	copy(out[outPos:outPos+4], in[inPos:inPos+4])
	return true
}
