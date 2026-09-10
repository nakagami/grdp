package rdpgfx

import (
	"math/rand"
	"testing"
	"unsafe"
)

func ictToBGRASlow(yRow, cbRow, crRow []int16, dst []byte, n int) {
	for col := 0; col < n; col++ {
		yv := int32(yRow[col])
		cb := int32(cbRow[col])
		cr := int32(crRow[col])
		ys := (yv + 4096) << 16
		bv := uint32(max(0, min((cb*115992+ys)>>21, 255)))
		gv := uint32(max(0, min((ys-cb*22527-cr*46819)>>21, 255)))
		rv := uint32(max(0, min((cr*91916+ys)>>21, 255)))
		*(*uint32)(unsafe.Pointer(&dst[col*4])) = bv | gv<<8 | rv<<16 | 0xFF000000
	}
}

func TestIctToBGRA_NEONvsSlow(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	n := 64
	yRow := make([]int16, n)
	cbRow := make([]int16, n)
	crRow := make([]int16, n)
	dst1 := make([]byte, n*4)
	dst2 := make([]byte, n*4)

	for iter := 0; iter < 1000; iter++ {
		for i := 0; i < n; i++ {
			yRow[i] = int16(rng.Intn(8192) - 4096)
			cbRow[i] = int16(rng.Intn(4096) - 2048)
			crRow[i] = int16(rng.Intn(4096) - 2048)
		}
		ictToBGRA(yRow, cbRow, crRow, dst1, n)
		ictToBGRASlow(yRow, cbRow, crRow, dst2, n)
		for i := 0; i < n*4; i++ {
			if dst1[i] != dst2[i] {
				t.Fatalf("mismatch at iter %d index %d: NEON=%d slow=%d (y=%d cb=%d cr=%d)",
					iter, i, dst1[i], dst2[i], yRow[i/4], cbRow[i/4], crRow[i/4])
			}
		}
	}
}
