package rdpgfx

import (
	"encoding/binary"
	"testing"
)

func TestNSCodecDecode(t *testing.T) {
	w, h := 8, 4

	// Plain color in YCoCg:
	// Let R=100, G=150, B=200
	// Y = (R + 2*G + B) / 4 = (100 + 300 + 200) / 4 = 150
	// Co = (R - B) = 100 - 200 = -100
	// Cg = G - (R + B)/2 = 150 - 150 = 0
	// In unsigned byte with shift=0 (colorLossLevel=1):
	// Y byte = 150
	// Co byte = uint8(int8(-100)) = 156
	// Cg byte = uint8(int8(0)) = 0
	// A byte = 255

	rw := (w + 7) &^ 7
	rh := (h + 1) &^ 1

	yLen := rw * h
	coLen := (rw / 2) * (rh / 2)
	cgLen := (rw / 2) * (rh / 2)
	aLen := w * h

	yPlane := make([]byte, yLen)
	for i := range yPlane {
		yPlane[i] = 150
	}
	coPlane := make([]byte, coLen)
	for i := range coPlane {
		coPlane[i] = byte(256 - 100)
	}
	cgPlane := make([]byte, cgLen)
	for i := range cgPlane {
		cgPlane[i] = 0
	}
	aPlane := make([]byte, aLen)
	for i := range aPlane {
		aPlane[i] = 255
	}

	// Construct uncompressed NSCodec payload:
	// Header (20 bytes):
	// L0 = yLen, L1 = coLen, L2 = cgLen, L3 = aLen
	// colorLossLevel = 1, chromaSubsampling = 1, reserved = 0
	payload := make([]byte, 20+yLen+coLen+cgLen+aLen)
	binary.LittleEndian.PutUint32(payload[0:], uint32(yLen))
	binary.LittleEndian.PutUint32(payload[4:], uint32(coLen))
	binary.LittleEndian.PutUint32(payload[8:], uint32(cgLen))
	binary.LittleEndian.PutUint32(payload[12:], uint32(aLen))
	payload[16] = 1 // colorLossLevel
	payload[17] = 1 // chromaSubsampling

	off := 20
	copy(payload[off:], yPlane)
	off += yLen
	copy(payload[off:], coPlane)
	off += coLen
	copy(payload[off:], cgPlane)
	off += cgLen
	copy(payload[off:], aPlane)

	out := make([]byte, w*h*4)
	ok := decodeNSCodec(payload, w, h, out, 0, 0, w, h)
	if !ok {
		t.Fatalf("decodeNSCodec failed")
	}

	// Verify pixel colors
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := (y*w + x) * 4
			b := out[idx]
			g := out[idx+1]
			r := out[idx+2]
			a := out[idx+3]

			// Expected:
			// r_val = 150 + (-100) - 0 = 50 -> wait!
			// For Y=150, Co=-100, Cg=0:
			// r = 150 + (-100) - 0 = 50
			// g = 150 + 0 = 150
			// b = 150 - (-100) - 0 = 250
			if b != 250 || g != 150 || r != 50 || a != 255 {
				t.Fatalf("pixel (%d,%d): expected (250,150,50,255), got (%d,%d,%d,%d)", x, y, b, g, r, a)
			}
		}
	}
}

func TestNSCodecRLE(t *testing.T) {
	// Original data: 10 bytes: [5, 5, 5, 5, 5, 5, 1, 2, 3, 4]
	// RLE encoded:
	// '5', '5', runLen-2 = 4 (byte value 4) -> 6 fives
	// '1', '2', '3', '4' (last 4 bytes copied literally)
	// Encoded stream: [5, 5, 4, 1, 2, 3, 4]
	encoded := []byte{5, 5, 4, 1, 2, 3, 4}
	out := make([]byte, 10)
	ok := nscRLEDecode(encoded, out, 10)
	if !ok {
		t.Fatalf("nscRLEDecode failed")
	}
	expected := []byte{5, 5, 5, 5, 5, 5, 1, 2, 3, 4}
	for i := range expected {
		if out[i] != expected[i] {
			t.Errorf("at %d: expected %d, got %d", i, expected[i], out[i])
		}
	}
}
