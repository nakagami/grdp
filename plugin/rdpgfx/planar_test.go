package rdpgfx

import (
	"testing"
)

func TestDecodePlanar_Raw_NoAlpha(t *testing.T) {
	w, h := 2, 2
	// Header: CLL=0, CS=0, RLE=0, NA=1 => 0x20
	header := byte(planarHeaderNA) // 0x20
	// 4 bytes per plane (w*h = 4): Red, Green, Blue
	red := []byte{10, 20, 30, 40}
	green := []byte{50, 60, 70, 80}
	blue := []byte{90, 100, 110, 120}

	data := append([]byte{header}, red...)
	data = append(data, green...)
	data = append(data, blue...)

	out := decodePlanar(data, w, h)
	defer releaseBitmapBuf(out)

	if len(out) != w*h*4 {
		t.Fatalf("expected len %d, got %d", w*h*4, len(out))
	}

	for i := 0; i < 4; i++ {
		b := out[i*4]
		g := out[i*4+1]
		r := out[i*4+2]
		a := out[i*4+3]
		if b != blue[i] || g != green[i] || r != red[i] || a != 0xFF {
			t.Errorf("pixel %d: expected (%d,%d,%d,255), got (%d,%d,%d,%d)",
				i, blue[i], green[i], red[i], b, g, r, a)
		}
	}
}

func TestDecodePlanar_Raw_WithAlpha(t *testing.T) {
	w, h := 2, 2
	// Header: CLL=0, CS=0, RLE=0, NA=0 => 0x00
	header := byte(0)
	alpha := []byte{200, 210, 220, 230}
	red := []byte{10, 20, 30, 40}
	green := []byte{50, 60, 70, 80}
	blue := []byte{90, 100, 110, 120}

	data := append([]byte{header}, alpha...)
	data = append(data, red...)
	data = append(data, green...)
	data = append(data, blue...)

	out := decodePlanar(data, w, h)
	defer releaseBitmapBuf(out)

	for i := 0; i < 4; i++ {
		b := out[i*4]
		g := out[i*4+1]
		r := out[i*4+2]
		a := out[i*4+3]
		if b != blue[i] || g != green[i] || r != red[i] || a != alpha[i] {
			t.Errorf("pixel %d: expected (%d,%d,%d,%d), got (%d,%d,%d,%d)",
				i, blue[i], green[i], red[i], alpha[i], b, g, r, a)
		}
	}
}

func TestDecodePlanar_RLE_NoAlpha(t *testing.T) {
	// 4x2 image (w=4, h=2)
	// Plane Red:
	//   row 0: [10, 10, 10, 10] -> ctrl: rawBytes=1, runLen=3 => 0x13, val=10
	//   row 1: [10, 10, 10, 10] -> delta=0 relative to row 0 -> ctrl: rawBytes=1, runLen=3 => 0x13, delta=0 (even => 0)
	// Plane Green:
	//   row 0: [20, 21, 22, 23] -> ctrl: rawBytes=4, runLen=0 => 0x40, vals=[20, 21, 22, 23]
	//   row 1: [21, 22, 23, 24] -> delta=+1 relative to row 0 -> ctrl: rawBytes=1, runLen=3 => 0x13, delta=2 (+1 encoded as 2)
	// Plane Blue:
	//   row 0: [30, 30, 30, 30] -> ctrl: 0x13, val=30
	//   row 1: [29, 29, 29, 29] -> delta=-1 relative to row 0 -> ctrl: 0x13, delta=1 (-1 encoded as odd 1)

	w, h := 4, 2
	header := byte(planarHeaderRLE | planarHeaderNA) // 0x30

	redRLE := []byte{0x13, 10, 0x13, 0}
	greenRLE := []byte{0x40, 20, 21, 22, 23, 0x13, 2}
	blueRLE := []byte{0x13, 30, 0x13, 1}

	data := []byte{header}
	data = append(data, redRLE...)
	data = append(data, greenRLE...)
	data = append(data, blueRLE...)

	out := decodePlanar(data, w, h)
	defer releaseBitmapBuf(out)

	// Expected row 0:
	// x=0: R=10, G=20, B=30
	// x=1: R=10, G=21, B=30
	// x=2: R=10, G=22, B=30
	// x=3: R=10, G=23, B=30
	// Expected row 1:
	// x=0: R=10, G=21, B=29
	// x=1: R=10, G=22, B=29
	// x=2: R=10, G=23, B=29
	// x=3: R=10, G=24, B=29

	expected := [][3]byte{
		{30, 20, 10}, {30, 21, 10}, {30, 22, 10}, {30, 23, 10},
		{29, 21, 10}, {29, 22, 10}, {29, 23, 10}, {29, 24, 10},
	}

	for i, exp := range expected {
		b := out[i*4]
		g := out[i*4+1]
		r := out[i*4+2]
		a := out[i*4+3]
		if b != exp[0] || g != exp[1] || r != exp[2] || a != 0xFF {
			t.Errorf("pixel %d: expected BGR=(%d,%d,%d), got (%d,%d,%d)", i, exp[0], exp[1], exp[2], b, g, r)
		}
	}
}

func TestDecodePlaneRLE_RunLength1And2(t *testing.T) {
	// w=20, h=1
	// Scanline 0:
	// ctrl: rawBytes=4, runLen=1 => runLen = 4 + 16 = 20, rawBytes = 0.
	// Since rawBytes=0, pixel=0. All 20 pixels get 0.
	data := []byte{0x41}
	out, _ := decodePlaneRLE(data, 0, 20, 1)
	defer releasePlaneBuf(out)

	for i := 0; i < 20; i++ {
		if out[i] != 0 {
			t.Errorf("expected out[%d]=0, got %d", i, out[i])
		}
	}
}
