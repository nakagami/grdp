package rdpgfx

import "testing"

// A match distance beyond the history window must be rejected; adding
// zgfxHistorySize once would leave the source index negative and panic.
func TestZgfxOutputMatchRejectsOversizedDistance(t *testing.T) {
	z := newZgfxContext()
	var out []byte
	z.outputMatch(zgfxHistorySize+1, 10, &out)
	if len(out) != 0 {
		t.Fatalf("expected no output for out-of-range distance, got %d bytes", len(out))
	}
	// A distance exactly at the window size is valid (wraps to index 0).
	z.history[0] = 0xAB
	z.outputMatch(zgfxHistorySize, 1, &out)
	if len(out) != 1 || out[0] != 0xAB {
		t.Fatalf("expected wrapped history byte 0xAB, got %v", out)
	}
}

// Truncated streams must fail cleanly instead of underflowing bitsRemaining
// (uint32 wraparound) and producing a huge bogus match count.
func TestZgfxDecodeMatchCountTruncated(t *testing.T) {
	z := newZgfxContext()

	br := &bitReader{} // no data, no decodable bits
	if _, ok := z.decodeMatchCount(br); ok {
		t.Fatal("expected failure on empty stream")
	}
	if br.bitsRemaining != 0 {
		t.Fatalf("bitsRemaining underflowed: %d", br.bitsRemaining)
	}

	// All-ones prefix bits: every leading 1 doubles the count; the reader
	// must bail out once the bits run out instead of underflowing.
	br = &bitReader{data: []byte{0xFF, 0xFF}, bitPos: 8, bitsRemaining: 16}
	if _, ok := z.decodeMatchCount(br); ok {
		t.Fatal("expected failure when prefix bits exceed remaining input")
	}
}
