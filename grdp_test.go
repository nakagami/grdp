package grdp

import "testing"

// 15 bpp sources must decode as RGB555 (5-5-5), not RGB565.
// RGB555 pure red 0x7C00, big-endian on the wire.
func TestFillBGRA15bpp(t *testing.T) {
	bm := Bitmap{Width: 1, Height: 1, BitsPerPixel: 2, SrcBitsPerPixel: 15, Data: []byte{0x7C, 0x00}}
	out := bm.FillBGRA(nil)
	if out[2] != 0xF8 || out[0] != 0 || out[1] != 0 || out[3] != 0xFF {
		t.Errorf("15bpp red decoded as %v, want BGRA(0,0,0xF8,0xFF)", out)
	}
}

// 16 bpp sources stay RGB565 (5-6-5).  RGB565 pure green 0x07E0.
func TestFillBGRA16bpp(t *testing.T) {
	bm := Bitmap{Width: 1, Height: 1, BitsPerPixel: 2, SrcBitsPerPixel: 16, Data: []byte{0x07, 0xE0}}
	out := bm.FillBGRA(nil)
	if out[1] != 0xFC || out[0] != 0 || out[2] != 0 || out[3] != 0xFF {
		t.Errorf("16bpp green decoded as %v, want BGRA(0,0xFC,0,0xFF)", out)
	}
}

// SrcBitsPerPixel == 0 must behave as 16 bpp (RGB565) for 2-byte pixels.
func TestFillBGRADefaultSrcDepth(t *testing.T) {
	bm := Bitmap{Width: 1, Height: 1, BitsPerPixel: 2, Data: []byte{0x07, 0xE0}}
	out := bm.FillBGRA(nil)
	if out[1] != 0xFC {
		t.Errorf("default depth decoded as %v, want G=0xFC", out)
	}
}

// Unsupported depths (BitsPerPixel 0 = 1/4/8 bpp indexed) must return a
// zeroed buffer without panicking (previously a divide-by-zero).
func TestFillBGRAUnsupportedDepth(t *testing.T) {
	bm := Bitmap{Width: 2, Height: 2, BitsPerPixel: 0, Data: []byte{1, 2, 3}}
	out := bm.FillBGRA(nil)
	if len(out) != 16 {
		t.Fatalf("len = %d, want 16", len(out))
	}
	for i, b := range out {
		if b != 0 {
			t.Fatalf("out[%d] = %d, want 0", i, b)
		}
	}
	if rgba := bm.FillRGBA(nil); rgba == nil {
		t.Fatal("FillRGBA returned nil for unsupported depth")
	}
}
