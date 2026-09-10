package rdpgfx

import (
	"encoding/binary"
	"testing"
)

func TestClearCodecResidual(t *testing.T) {
	w, h := 4, 2
	// Residual data: 2 runs
	// Run 1: B=10, G=20, R=30, len=5
	// Run 2: B=40, G=50, R=60, len=3
	resData := []byte{
		10, 20, 30, 5,
		40, 50, 60, 3,
	}

	payload := make([]byte, 2+12+len(resData))
	payload[0] = 0 // flags
	payload[1] = 0 // seq
	binary.LittleEndian.PutUint32(payload[2:], uint32(len(resData))) // residualLen
	binary.LittleEndian.PutUint32(payload[6:], 0)                   // bandsLen
	binary.LittleEndian.PutUint32(payload[10:], 0)                  // subcodecLen
	copy(payload[14:], resData)

	ctx := newClearCodecCtx()
	out := ctx.decode(payload, w, h)
	if len(out) != w*h*4 {
		t.Fatalf("expected output len %d, got %d", w*h*4, len(out))
	}

	for i := 0; i < 5; i++ {
		b := out[i*4]
		g := out[i*4+1]
		r := out[i*4+2]
		a := out[i*4+3]
		if b != 10 || g != 20 || r != 30 || a != 0xFF {
			t.Errorf("pixel %d: expected (10,20,30,255), got (%d,%d,%d,%d)", i, b, g, r, a)
		}
	}
	for i := 5; i < 8; i++ {
		b := out[i*4]
		g := out[i*4+1]
		r := out[i*4+2]
		a := out[i*4+3]
		if b != 40 || g != 50 || r != 60 || a != 0xFF {
			t.Errorf("pixel %d: expected (40,50,60,255), got (%d,%d,%d,%d)", i, b, g, r, a)
		}
	}
}

func TestClearCodecBands(t *testing.T) {
	ctx := newClearCodecCtx()
	w, h := 4, 4

	// Create a band from x=0..1, y=0..3 (2 columns, height 4)
	// Background color: B=1, G=2, R=3
	// Column 0: Short VBar Cache Miss
	//   vBarYOn = 1, vBarYOff = 3 => short pixel count = 2
	//   short pixels: (10, 11, 12), (20, 21, 22)
	// Column 1: Short VBar Cache Hit
	//   vBarIndex = 0, vBarYOn = 2 => short pixels placed at y=2..3
	bandsData := []byte{
		0, 0, // xStart = 0
		1, 0, // xEnd = 1
		0, 0, // yStart = 0
		3, 0, // yEnd = 3
		1, 2, 3, // B, G, R background
	}

	// Column 0: SHORT_VBAR_CACHE_MISS
	// Header: bits 15..14 = 00. bits 0..7 = yOn (1), bits 8..13 = yOff (3) => 0x0301
	vBar0 := uint16(1 | (3 << 8))
	bandsData = binary.LittleEndian.AppendUint16(bandsData, vBar0)
	bandsData = append(bandsData,
		10, 11, 12, // pixel 0
		20, 21, 22, // pixel 1
	)

	// Column 1: SHORT_VBAR_CACHE_HIT
	// Header: bits 15..14 = 01 (0x4000). Index = 0 => 0x4000
	vBar1 := uint16(0x4000 | 0)
	bandsData = binary.LittleEndian.AppendUint16(bandsData, vBar1)
	bandsData = append(bandsData, 2) // yOn = 2

	payload := make([]byte, 2+12+len(bandsData))
	payload[0] = 0
	payload[1] = 0
	binary.LittleEndian.PutUint32(payload[2:], 0)                    // residualLen
	binary.LittleEndian.PutUint32(payload[6:], uint32(len(bandsData))) // bandsLen
	binary.LittleEndian.PutUint32(payload[10:], 0)                  // subcodecLen
	copy(payload[14:], bandsData)

	out := ctx.decode(payload, w, h)

	// Check Col 0 (x=0):
	// y=0: bg (1,2,3)
	// y=1: (10,11,12)
	// y=2: (20,21,22)
	// y=3: bg (1,2,3)
	checkPixel := func(x, y int, eb, eg, er uint8) {
		idx := (y*w + x) * 4
		b, g, r, a := out[idx], out[idx+1], out[idx+2], out[idx+3]
		if b != eb || g != eg || r != er || a != 0xFF {
			t.Errorf("pixel (%d,%d): expected (%d,%d,%d,255), got (%d,%d,%d,%d)", x, y, eb, eg, er, b, g, r, a)
		}
	}
	checkPixel(0, 0, 1, 2, 3)
	checkPixel(0, 1, 10, 11, 12)
	checkPixel(0, 2, 20, 21, 22)
	checkPixel(0, 3, 1, 2, 3)

	// Check Col 1 (x=1):
	// y=0: bg (1,2,3)
	// y=1: bg (1,2,3)
	// y=2: (10,11,12)
	// y=3: (20,21,22)
	checkPixel(1, 0, 1, 2, 3)
	checkPixel(1, 1, 1, 2, 3)
	checkPixel(1, 2, 10, 11, 12)
	checkPixel(1, 3, 20, 21, 22)
}

func TestClearCodecSubcodecRLEX(t *testing.T) {
	ctx := newClearCodecCtx()
	w, h := 4, 2

	// Subcodec RLEX:
	// palette count = 2: (100, 150, 200), (50, 60, 70)
	// Palette 0: (100,150,200), Palette 1: (50,60,70)
	// numBits = log2floor[1] + 1 = 1
	// item 1: runLengthFactor=3, palette 0 => 3 pixels of color 0 + suiteDepth 0 of color 0 => total 4 pixels
	// tmp byte: stopIndex=0, suiteDepth=0 => 0
	// item 2: runLengthFactor=3, palette 1 => 3 pixels of color 1 + suiteDepth 0 of color 1 => total 4 pixels
	// tmp byte: stopIndex=1, suiteDepth=0 => 1
	bmpData := []byte{
		2, // paletteCount
		100, 150, 200, // color 0 (B,G,R)
		50, 60, 70, // color 1 (B,G,R)
		0, 3, // tmp=0, runLen=3
		1, 3, // tmp=1, runLen=3
	}

	subcodecPayload := make([]byte, 13+len(bmpData))
	binary.LittleEndian.PutUint16(subcodecPayload[0:], 0) // xStart
	binary.LittleEndian.PutUint16(subcodecPayload[2:], 0) // yStart
	binary.LittleEndian.PutUint16(subcodecPayload[4:], uint16(w))
	binary.LittleEndian.PutUint16(subcodecPayload[6:], uint16(h))
	binary.LittleEndian.PutUint32(subcodecPayload[8:], uint32(len(bmpData)))
	subcodecPayload[12] = 2 // RLEX
	copy(subcodecPayload[13:], bmpData)

	payload := make([]byte, 2+12+len(subcodecPayload))
	payload[0] = 0
	payload[1] = 0
	binary.LittleEndian.PutUint32(payload[2:], 0)                        // residualLen
	binary.LittleEndian.PutUint32(payload[6:], 0)                        // bandsLen
	binary.LittleEndian.PutUint32(payload[10:], uint32(len(subcodecPayload))) // subcodecLen
	copy(payload[14:], subcodecPayload)

	out := ctx.decode(payload, w, h)
	for i := 0; i < 4; i++ {
		idx := i * 4
		if out[idx] != 100 || out[idx+1] != 150 || out[idx+2] != 200 {
			t.Errorf("pixel %d: expected (100,150,200), got (%d,%d,%d)", i, out[idx], out[idx+1], out[idx+2])
		}
	}
	for i := 4; i < 8; i++ {
		idx := i * 4
		if out[idx] != 50 || out[idx+1] != 60 || out[idx+2] != 70 {
			t.Errorf("pixel %d: expected (50,60,70), got (%d,%d,%d)", i, out[idx], out[idx+1], out[idx+2])
		}
	}
}
