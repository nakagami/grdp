package rdpgfx

import (
	"encoding/binary"
	"encoding/hex"
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

	pixels, raw, sign := rfxDecodeComponentProgressive(data, shift, nil, false, true)
	defer coeffPool.Put((*coeffArr)(pixels))
	defer coeffPool.Put(raw)
	defer coeffPool.Put(sign)

	if len(pixels) != 4096 {
		t.Fatalf("expected 4096 pixels, got %d", len(pixels))
	}
	for i, v := range pixels {
		if v != 16 {
			t.Fatalf("expected pixel[%d] == 16, got %d", i, v)
		}
	}
}

// TestRfxProgressive_DumpTile tests decoding actual tile data dumped from a session.
func TestRfxProgressive_DumpTile(t *testing.T) {
	yHex := "0000178a6094a881a2e824a4646b3222606925a260026ffffffffffffffffffffffffffffe8400500090010002c00300140080000800040001000010100000c02801001000c00500821168d68440"
	cbHex := "0000080900"
	crHex := "0000080900"
	yData, _ := hex.DecodeString(yHex)
	cbData, _ := hex.DecodeString(cbHex)
	crData, _ := hex.DecodeString(crHex)

	shiftY := rfxQuant{LL3: 5, LH3: 5, HL3: 5, HH3: 6, LH2: 7, HL2: 7, HH2: 8, LH1: 8, HL1: 8, HH1: 9}
	shiftCb := rfxQuant{LL3: 5, LH3: 5, HL3: 5, HH3: 6, LH2: 7, HL2: 7, HH2: 8, LH1: 8, HL1: 8, HH1: 9}
	shiftCr := rfxQuant{LL3: 5, LH3: 5, HL3: 5, HH3: 6, LH2: 7, HL2: 7, HH2: 8, LH1: 8, HL1: 8, HH1: 9}

	// Try both extrapolate = false and extrapolate = true
	for _, ext := range []bool{false, true} {
		yPix, rawY, signY := rfxDecodeComponentProgressive(yData, shiftY, nil, false, ext)
		cbPix, rawCb, signCb := rfxDecodeComponentProgressive(cbData, shiftCb, nil, false, ext)
		crPix, rawCr, signCr := rfxDecodeComponentProgressive(crData, shiftCr, nil, false, ext)

		out := make([]byte, 64*64*4)
		rfxPlaceTile(yPix, cbPix, crPix, 0, 0, out, 64, 64)

		t.Logf("extrapolate=%v: first pixel BGRA=(%d,%d,%d,%d) center BGRA=(%d,%d,%d,%d)",
			ext, out[0], out[1], out[2], out[3], out[2048*4], out[2048*4+1], out[2048*4+2], out[2048*4+3])

		coeffPool.Put((*coeffArr)(yPix))
		coeffPool.Put((*coeffArr)(cbPix))
		coeffPool.Put((*coeffArr)(crPix))
		coeffPool.Put(rawY)
		coeffPool.Put(rawCb)
		coeffPool.Put(rawCr)
		coeffPool.Put(signY)
		coeffPool.Put(signCb)
		coeffPool.Put(signCr)
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

// TestRfxProgressiveDecoder_TileUpgrade tests TileFirst followed by TileUpgrade.
func TestRfxProgressiveDecoder_TileUpgrade(t *testing.T) {
	dec := newRfxProgressiveDecoder()
	surf := make([]byte, 64*64*4)

	// 1. Send TileFirst (quality = 0)
	// quant: all 6s (0x66), progQuant: all 2s
	yCoeffs := make([]int16, 4096)
	yCoeffs[4015] = 16
	yData := rlgr1Encode(yCoeffs)

	tileFirstBody := make([]byte, 17+len(yData))
	tileFirstBody[0] = 0 // quantIdxY
	tileFirstBody[1] = 0 // quantIdxCb
	tileFirstBody[2] = 0 // quantIdxCr
	tileFirstBody[8] = 0 // quality = 0
	tileFirstBody[9] = byte(len(yData))
	tileFirstBody[10] = byte(len(yData) >> 8)
	copy(tileFirstBody[17:], yData)

	tileFirstHeader := make([]byte, 6)
	binary.LittleEndian.PutUint16(tileFirstHeader[0:], progWBTTileFirst)
	binary.LittleEndian.PutUint32(tileFirstHeader[2:], uint32(6+len(tileFirstBody)))

	region1Data := make([]byte, 0, 1024)
	region1Hdr := make([]byte, 12)
	region1Hdr[0] = 0x40
	binary.LittleEndian.PutUint16(region1Hdr[1:], 1) // numRects
	region1Hdr[3] = 1 // numQuant
	region1Hdr[4] = 1 // numProgQuant
	binary.LittleEndian.PutUint16(region1Hdr[6:], 1) // numTiles
	binary.LittleEndian.PutUint32(region1Hdr[8:], uint32(6+len(tileFirstBody)))
	region1Data = append(region1Data, region1Hdr...)
	region1Data = append(region1Data, 0, 0, 0, 0, 64, 0, 64, 0) // rect
	region1Data = append(region1Data, 0x66, 0x66, 0x66, 0x66, 0x66) // quant: 6
	progQuantBytes := make([]byte, 16)
	progQuantBytes[0] = 0 // quality = 0
	for i := 1; i < 16; i++ { progQuantBytes[i] = 0x22 } // progQuant = 2
	region1Data = append(region1Data, progQuantBytes...)
	region1Data = append(region1Data, tileFirstHeader...)
	region1Data = append(region1Data, tileFirstBody...)

	stream1 := make([]byte, 6+len(region1Data))
	binary.LittleEndian.PutUint16(stream1[0:], progWBTRegion)
	binary.LittleEndian.PutUint32(stream1[2:], uint32(6+len(region1Data)))
	copy(stream1[6:], region1Data)

	rects := dec.Decode(stream1, surf, 64, 64)
	if len(rects) == 0 {
		t.Fatalf("TileFirst: expected decoded rects, got none")
	}

	// 2. Send TileUpgrade (quality = 0xFF, full quality)
	// 20-byte header: quantIdxY(1), quantIdxCb(1), quantIdxCr(1), xIdx(2), yIdx(2), quality(1), ySrlLen(2), yRawLen(2), cbSrlLen(2), cbRawLen(2), crSrlLen(2), crRawLen(2)
	rawBytes := make([]byte, 20)
	tileUpBody := make([]byte, 20+len(rawBytes))
	tileUpBody[0] = 0 // quantIdxY
	tileUpBody[1] = 0
	tileUpBody[2] = 0
	tileUpBody[7] = 0xFF // quality = 0xFF
	tileUpBody[8] = 0    // ySrlLen = 0
	tileUpBody[10] = byte(len(rawBytes)) // yRawLen
	tileUpBody[11] = byte(len(rawBytes) >> 8)
	copy(tileUpBody[20:], rawBytes)

	tileUpHeader := make([]byte, 6)
	binary.LittleEndian.PutUint16(tileUpHeader[0:], progWBTTileUpgrade)
	binary.LittleEndian.PutUint32(tileUpHeader[2:], uint32(6+len(tileUpBody)))

	region2Data := make([]byte, 0, 1024)
	region2Hdr := make([]byte, 12)
	region2Hdr[0] = 0x40
	binary.LittleEndian.PutUint16(region2Hdr[1:], 1)
	region2Hdr[3] = 1
	region2Hdr[4] = 0
	binary.LittleEndian.PutUint16(region2Hdr[6:], 1)
	binary.LittleEndian.PutUint32(region2Hdr[8:], uint32(6+len(tileUpBody)))
	region2Data = append(region2Data, region2Hdr...)
	region2Data = append(region2Data, 0, 0, 0, 0, 64, 0, 64, 0)
	region2Data = append(region2Data, 0x66, 0x66, 0x66, 0x66, 0x66)
	region2Data = append(region2Data, tileUpHeader...)
	region2Data = append(region2Data, tileUpBody...)

	stream2 := make([]byte, 6+len(region2Data))
	binary.LittleEndian.PutUint16(stream2[0:], progWBTRegion)
	binary.LittleEndian.PutUint32(stream2[2:], uint32(6+len(region2Data)))
	copy(stream2[6:], region2Data)

	rects2 := dec.Decode(stream2, surf, 64, 64)
	if len(rects2) == 0 {
		t.Fatalf("TileUpgrade: expected decoded rects, got none")
	}
	if surf[3] != 0xFF {
		t.Errorf("expected surf[3] == 0xFF after upgrade, got %x", surf[3])
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

