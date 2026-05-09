package rdpgfx

import "sync"

// Buffer pools for RFX tile decoding to minimize allocations in the hot path.
// Each 64×64 tile needs 4096 int16 coefficients per component (Y, Cb, Cr)
// and several temporary buffers for the IDWT.

var coeffPool = sync.Pool{
	New: func() any { return make([]int16, 4096) },
}

// idwtBufs holds the temporary buffer for one rfxIDWT2DLevel call.
// The subbands (HL/LH/HH/LL) are read directly from the input buf without copying.
type idwtBufs struct {
	tmp []int16 // intermediate row-interleaved buffer; max 64×64 = 4096
}

var idwtBufPool = sync.Pool{
	New: func() any {
		return &idwtBufs{tmp: make([]int16, 4096)}
	},
}
