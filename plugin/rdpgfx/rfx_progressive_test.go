package rdpgfx

import (
	"encoding/binary"
	"testing"
)

// BenchmarkIctToBGRA benchmarks ictToBGRA on a full 64×64 tile row (64 pixels).
func BenchmarkIctToBGRA(b *testing.B) {
	n := rfxTileSize // 64 pixels per row
	yRow := make([]int16, n)
	cbRow := make([]int16, n)
	crRow := make([]int16, n)
	dst := make([]byte, n*4)
	for i := range n {
		yRow[i] = int16(i * 4)
		cbRow[i] = int16(i%64 - 32)
		crRow[i] = int16(i%32 - 16)
	}
	b.ResetTimer()
	for b.Loop() {
		ictToBGRA(yRow, cbRow, crRow, dst, n)
	}
}

// BenchmarkRfxDecodeComponent benchmarks a full component decode pipeline:
// RLGR → differential/dequantize → inverse DWT.
func BenchmarkRfxDecodeComponent(b *testing.B) {
	// Use the same coefficient pattern as the RLGR benchmarks (1/17 non-zero).
	coeffs := make([]int16, 4096)
	for i := range coeffs {
		if i%17 == 0 {
			coeffs[i] = int16(i%256 - 128)
		}
	}
	data := rlgr1Encode(coeffs)
	quant := rfxQuant{6, 6, 6, 6, 6, 6, 6, 6, 6, 6}

	b.ResetTimer()
	var dst []int16
	for b.Loop() {
		dst = rfxDecodeComponent(data, quant, 1)
		coeffPool.Put((*coeffArr)(dst))
		dst = nil
	}
}

// TestRfxDecodeComponentProgressive tests decoding a component with extrapolated IDWT.
func TestRfxDecodeComponentProgressive(t *testing.T) {
	coeffs := make([]int16, 4096)
	coeffs[4015] = 16
	data := rlgr1Encode(coeffs)

	// shift with all 0s (no shifts)
	shift := rfxQuant{}

	pixels, raw := rfxDecodeComponentProgressive(data, shift, nil, false)
	defer coeffPool.Put((*coeffArr)(pixels))
	defer coeffPool.Put(raw)

	if len(pixels) != 4096 {
		t.Fatalf("expected 4096 pixels, got %d", len(pixels))
	}
	for i, v := range pixels {
		if v != 16 {
			t.Fatalf("expected pixel[%d] == 16, got %d", i, v)
		}
	}
}

// TestRfxProgressiveDecoder_TileSimple tests end-to-end decoding of a TileSimple block.
func TestRfxProgressiveDecoder_TileSimple(t *testing.T) {
	dec := newRfxProgressiveDecoder()
	surf := make([]byte, 64*64*4)

	// Build a PROGRESSIVE_WBT_REGION containing 1 TILE_SIMPLE
	// Region header (12 bytes):
	// tileSize: 0x40
	// numRects: 1 (uint16)
	// numQuant: 1 (uint8)
	// numProgQuant: 0 (uint8)
	// flags: 0 (uint8)
	// numTiles: 1 (uint16)
	// tileDataSize: len(tile) (uint32)

	// 1 Rect: [0, 0, 64, 64] (8 bytes)
	// 1 Quant: [6, 6, 6, 6, 6] (5 bytes: 0x66, 0x66, 0x66, 0x66, 0x66)
	// 0 ProgQuant

	// 1 Tile:
	// blockType: progWBTTileSimple (0xCCC5) (2 bytes)
	// blockLen: 6 + tileBodyLen (4 bytes)
	// quantIdxY: 0, quantIdxCb: 0, quantIdxCr: 0
	// xIdx: 0, yIdx: 0
	// flags: 0
	// yLen, cbLen, crLen, tailLen

	yCoeffs := make([]int16, 4096)
	yCoeffs[4015] = 16
	yData := rlgr1Encode(yCoeffs)

	tileBody := make([]byte, 16+len(yData))
	tileBody[0] = 0 // quantIdxY
	tileBody[1] = 0 // quantIdxCb
	tileBody[2] = 0 // quantIdxCr
	// xIdx = 0, yIdx = 0
	// yLen:
	tileBody[8] = byte(len(yData))
	tileBody[9] = byte(len(yData) >> 8)
	copy(tileBody[16:], yData)

	tileHeader := make([]byte, 6)
	binary.LittleEndian.PutUint16(tileHeader[0:], progWBTTileSimple)
	tileLen := uint32(6 + len(tileBody))
	binary.LittleEndian.PutUint32(tileHeader[2:], tileLen)

	regionData := make([]byte, 0, 1024)
	regionHeader := make([]byte, 12)
	regionHeader[0] = 0x40 // tileSize
	binary.LittleEndian.PutUint16(regionHeader[1:], 1) // numRects = 1
	regionHeader[3] = 1    // numQuant = 1
	regionHeader[4] = 0    // numProgQuant = 0
	binary.LittleEndian.PutUint16(regionHeader[6:], 1) // numTiles = 1
	binary.LittleEndian.PutUint32(regionHeader[8:], tileLen)
	regionData = append(regionData, regionHeader...)

	// Rect [0, 0, 64, 64]
	rectBytes := []byte{0, 0, 0, 0, 64, 0, 64, 0}
	regionData = append(regionData, rectBytes...)

	// Quant (5 bytes): all nibbles 1 (shift = 0)
	quantBytes := []byte{0x11, 0x11, 0x11, 0x11, 0x11}
	regionData = append(regionData, quantBytes...)

	// Tile
	regionData = append(regionData, tileHeader...)
	regionData = append(regionData, tileBody...)

	// Full stream: WBT_REGION block (6 bytes header + regionData)
	stream := make([]byte, 6+len(regionData))
	binary.LittleEndian.PutUint16(stream[0:], progWBTRegion)
	streamLen := uint32(6 + len(regionData))
	binary.LittleEndian.PutUint32(stream[2:], streamLen)
	copy(stream[6:], regionData)

	rects := dec.Decode(stream, surf, 64, 64)
	if len(rects) == 0 {
		t.Fatalf("expected decoded rects, got none")
	}
	// Check that surface has been written to (alpha channel is 0xFF)
	if surf[3] != 0xFF {
		t.Errorf("expected surf[3] (alpha) == 0xFF, got %x", surf[3])
	}
}

// BenchmarkRfxInverseDWT2D benchmarks the full 3-level inverse DWT on 4096 coefficients.
func BenchmarkRfxInverseDWT2D(b *testing.B) {
	coeffs := make([]int16, 4096)
	for i := range coeffs {
		coeffs[i] = int16(i%64 - 32)
	}
	b.ResetTimer()
	for b.Loop() {
		rfxInverseDWT2D(coeffs)
	}
}

func TestExtrapolateDWT_MatchesFreeRDP(t *testing.T) {
	buf := make([]int16, 4096)
	temp := make([]int16, 4096)
	for i := range buf {
		if i%7 == 0 {
			buf[i] = int16(i%50) - 25
		}
	}
	buf[4015] = 100

	rfxInverseDWTExtrapolate(buf, temp)

	var sum uint32
	for i := range 4096 {
		sum = sum*31 + uint32(uint16(buf[i]))
	}
	expectedSum := uint32(20436711)
	if sum != expectedSum {
		t.Fatalf("expected checksum %d, got %d", expectedSum, sum)
	}
}

