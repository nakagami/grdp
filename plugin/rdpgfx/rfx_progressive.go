package rdpgfx

// RFX Progressive Codec decoder (MS-RDPRFX / MS-RDPEGFX 2.2.4).
// Handles RDPGFX_CODECID_CAPROGRESSIVE (0x0009) in WIRE_TO_SURFACE_PDU_2.

import (
	"encoding/binary"
	"log/slog"
	"runtime"
	"sync"
)

// Progressive block types (different from non-progressive WBT_* at same values!)
const (
	progWBTSync        = 0xCCC0
	progWBTFrameBegin  = 0xCCC1
	progWBTFrameEnd    = 0xCCC2
	progWBTContext     = 0xCCC3
	progWBTRegion      = 0xCCC4
	progWBTTileSimple  = 0xCCC5
	progWBTTileFirst   = 0xCCC6
	progWBTTileUpgrade = 0xCCC7
)

const rfxTileSize = 64

// rfxQuant holds the 10 quantization values for one component (5 bytes, 10 nibbles).
type rfxQuant struct {
	LL3, LH3, HL3, HH3 uint8
	LH2, HL2, HH2      uint8
	LH1, HL1, HH1      uint8
}

// rfxTileCoeffs holds the raw RLGR-decoded coefficients for one tile (all three
// components), stored before LL3 differential decode and dequantization.  This
// state is required to apply TILE_UPGRADE_FIRST delta data on top of a previous
// TILE_FIRST (or TILE_SIMPLE) pass.
type rfxTileCoeffs struct {
	y  []int16
	cb []int16
	cr []int16
}

type rfxProgressiveDecoder struct {
	mu        sync.Mutex
	tileCache map[uint32]*rfxTileCoeffs // key: yIdx<<16 | xIdx
}

func newRfxProgressiveDecoder() *rfxProgressiveDecoder {
	return &rfxProgressiveDecoder{
		tileCache: make(map[uint32]*rfxTileCoeffs),
	}
}

// Reset discards the tile coefficient cache.  Call this whenever the server
// starts a new progressive sequence (e.g. on RESET_GRAPHICS).
func (d *rfxProgressiveDecoder) Reset() {
	d.mu.Lock()
	d.tileCache = make(map[uint32]*rfxTileCoeffs)
	d.mu.Unlock()
}

// rfxRect represents a rectangle of decoded tiles.
type rfxRect struct {
	x, y, w, h int
}

// Decode processes RFX Progressive codec data, rendering tiles onto the
// provided surface buffer. Returns the bounding rectangles of decoded regions.
func (d *rfxProgressiveDecoder) Decode(data []byte, surfData []byte, width, height int) []rfxRect {
	var rects []rfxRect

	offset := 0
	for offset+6 <= len(data) {
		blockType := binary.LittleEndian.Uint16(data[offset:])
		blockLen := binary.LittleEndian.Uint32(data[offset+2:])

		if blockLen < 6 || offset+int(blockLen) > len(data) {
			break
		}

		blockData := data[offset+6 : offset+int(blockLen)]

		switch blockType {
		case progWBTSync, progWBTFrameBegin, progWBTFrameEnd, progWBTContext:
		// Infrastructure blocks — no action needed.
		case progWBTRegion:
			// Tiles are embedded inside the region block; parseRegion decodes them.
			regionRects, _ := d.parseRegion(blockData, surfData, width, height)
			rects = append(rects, regionRects...)
		default:
			slog.Debug("RFX: unknown progressive block type", "type", blockType)
		}

		offset += int(blockLen)
	}

	return rects
}

// parseRegion extracts rects and quant tables from a PROGRESSIVE_WBT_REGION block,
// and decodes the tile sub-blocks embedded within it onto the surface.
// Per MS-RDPEGFX 2.2.4, tile blocks (TILE_SIMPLE/TILE_FIRST) are sub-blocks
// inside the REGION block, not top-level stream blocks.
func (d *rfxProgressiveDecoder) parseRegion(data []byte, surfData []byte, outW, outH int) ([]rfxRect, []rfxQuant) {
	if len(data) < 12 {
		return nil, nil
	}

	// tileSize := data[0]
	numRects := binary.LittleEndian.Uint16(data[1:])
	numQuant := data[3]
	numProgQuant := data[4]
	// flags := data[5]
	numTiles := binary.LittleEndian.Uint16(data[6:])
	// tileDataSize := binary.LittleEndian.Uint32(data[8:])

	offset := 12

	// Parse rects (8 bytes each: x, y, width, height as uint16)
	rects := make([]rfxRect, numRects)
	for i := range numRects {
		if offset+8 > len(data) {
			return nil, nil
		}
		rx := int(binary.LittleEndian.Uint16(data[offset:]))
		ry := int(binary.LittleEndian.Uint16(data[offset+2:]))
		rw := int(binary.LittleEndian.Uint16(data[offset+4:]))
		rh := int(binary.LittleEndian.Uint16(data[offset+6:]))
		rects[i] = rfxRect{x: rx, y: ry, w: rw, h: rh}
		offset += 8
	}

	// Parse quant values (5 bytes each)
	quants := make([]rfxQuant, numQuant)
	for i := range numQuant {
		if offset+5 > len(data) {
			return nil, nil
		}
		quants[i] = parseRfxQuant(data[offset:])
		offset += 5
	}

	// Skip progressive quant values (RFX_PROGRESSIVE_CODEC_QUANT, 16 bytes each)
	offset += int(numProgQuant) * 16

	// Collect all decodable tiles before dispatching, so we can parallelise
	// when there are enough to amortise goroutine overhead (same threshold as
	// non-progressive decodeTileset in rfx.go).
	type progTileWork struct {
		tileType uint16
		data     []byte
	}
	tiles := make([]progTileWork, 0, numTiles)
	for offset+6 <= len(data) {
		tileType := binary.LittleEndian.Uint16(data[offset:])
		tileLen := binary.LittleEndian.Uint32(data[offset+2:])
		if tileLen < 6 || offset+int(tileLen) > len(data) {
			break
		}
		switch tileType {
		case progWBTTileSimple, progWBTTileFirst, progWBTTileUpgrade:
			tiles = append(tiles, progTileWork{tileType: tileType, data: data[offset+6 : offset+int(tileLen)]})
		default:
			slog.Debug("RFX: unknown progressive tile type", "type", tileType)
		}
		offset += int(tileLen)
	}

	const parallelTileThreshold = 12
	decodeTile := func(tw progTileWork, parallel bool) {
		switch tw.tileType {
		case progWBTTileSimple:
			d.decodeTileSimple(tw.data, quants, surfData, outW, outH, parallel)
		case progWBTTileFirst:
			d.decodeTileFirst(tw.data, quants, surfData, outW, outH, parallel)
		case progWBTTileUpgrade:
			d.decodeTileUpgrade(tw.data, quants, surfData, outW, outH, parallel)
		}
	}
	if len(tiles) >= parallelTileThreshold {
		workers := min(runtime.NumCPU(), len(tiles))
		ch := make(chan progTileWork, len(tiles))
		for _, tw := range tiles {
			ch <- tw
		}
		close(ch)
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("RFX progressive: tile decode panic", "err", r)
					}
				}()
				for tw := range ch {
					decodeTile(tw, false)
				}
			})
		}
		wg.Wait()
	} else {
		for _, tw := range tiles {
			decodeTile(tw, true)
		}
	}

	return rects, quants
}

func parseRfxQuant(data []byte) rfxQuant {
	return rfxQuant{
		LL3: data[0] & 0x0F,
		LH3: data[0] >> 4,
		HL3: data[1] & 0x0F,
		HH3: data[1] >> 4,
		LH2: data[2] & 0x0F,
		HL2: data[2] >> 4,
		HH2: data[3] & 0x0F,
		LH1: data[3] >> 4,
		HL1: data[4] & 0x0F,
		HH1: data[4] >> 4,
	}
}

// decodeTileSimple handles PROGRESSIVE_WBT_TILE_SIMPLE (0xCCC5).
// When parallelComponents is true the Y, Cb, and Cr channels are decoded
// concurrently. Use true for the serial-tile path.
func (d *rfxProgressiveDecoder) decodeTileSimple(data []byte, quants []rfxQuant, output []byte, outW, outH int, parallelComponents bool) {
	if len(data) < 16 {
		return
	}

	quantIdxY := data[0]
	quantIdxCb := data[1]
	quantIdxCr := data[2]
	xIdx := binary.LittleEndian.Uint16(data[3:])
	yIdx := binary.LittleEndian.Uint16(data[5:])
	// flags := data[7]
	yLen := binary.LittleEndian.Uint16(data[8:])
	cbLen := binary.LittleEndian.Uint16(data[10:])
	crLen := binary.LittleEndian.Uint16(data[12:])
	// tailLen := binary.LittleEndian.Uint16(data[14:])

	off := 16
	yData := safeSlice(data, off, int(yLen))
	off += int(yLen)
	cbData := safeSlice(data, off, int(cbLen))
	off += int(cbLen)
	crData := safeSlice(data, off, int(crLen))

	qY := rfxGetQuant(quants, int(quantIdxY))
	qCb := rfxGetQuant(quants, int(quantIdxCb))
	qCr := rfxGetQuant(quants, int(quantIdxCr))

	var yPixels, cbPixels, crPixels []int16
	var newY, newCb, newCr []int16
	if parallelComponents {
		var wg sync.WaitGroup
		wg.Go(func() { yPixels, newY = rfxDecodeComponentProgressive(yData, qY, nil) })
		wg.Go(func() { cbPixels, newCb = rfxDecodeComponentProgressive(cbData, qCb, nil) })
		wg.Go(func() { crPixels, newCr = rfxDecodeComponentProgressive(crData, qCr, nil) })
		wg.Wait()
	} else {
		yPixels, newY = rfxDecodeComponentProgressive(yData, qY, nil)
		cbPixels, newCb = rfxDecodeComponentProgressive(cbData, qCb, nil)
		crPixels, newCr = rfxDecodeComponentProgressive(crData, qCr, nil)
	}

	rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)

	tileKey := uint32(yIdx)<<16 | uint32(xIdx)
	d.mu.Lock()
	d.tileCache[tileKey] = &rfxTileCoeffs{y: newY, cb: newCb, cr: newCr}
	d.mu.Unlock()

	coeffPool.Put((*coeffArr)(yPixels))
	coeffPool.Put((*coeffArr)(cbPixels))
	coeffPool.Put((*coeffArr)(crPixels))
}

// decodeTileFirst handles PROGRESSIVE_WBT_TILE_FIRST (0xCCC6).
// When parallelComponents is true the Y, Cb, and Cr channels are decoded
// concurrently. Use true for the serial-tile path.
func (d *rfxProgressiveDecoder) decodeTileFirst(data []byte, quants []rfxQuant, output []byte, outW, outH int, parallelComponents bool) {
	if len(data) < 17 {
		return
	}

	quantIdxY := data[0]
	quantIdxCb := data[1]
	quantIdxCr := data[2]
	xIdx := binary.LittleEndian.Uint16(data[3:])
	yIdx := binary.LittleEndian.Uint16(data[5:])
	// flags := data[7]
	// quality := data[8]
	yLen := binary.LittleEndian.Uint16(data[9:])
	cbLen := binary.LittleEndian.Uint16(data[11:])
	crLen := binary.LittleEndian.Uint16(data[13:])
	// tailLen := binary.LittleEndian.Uint16(data[15:])

	off := 17
	yData := safeSlice(data, off, int(yLen))
	off += int(yLen)
	cbData := safeSlice(data, off, int(cbLen))
	off += int(cbLen)
	crData := safeSlice(data, off, int(crLen))

	qY := rfxGetQuant(quants, int(quantIdxY))
	qCb := rfxGetQuant(quants, int(quantIdxCb))
	qCr := rfxGetQuant(quants, int(quantIdxCr))

	var yPixels, cbPixels, crPixels []int16
	var newY, newCb, newCr []int16
	if parallelComponents {
		var wg sync.WaitGroup
		wg.Go(func() { yPixels, newY = rfxDecodeComponentProgressive(yData, qY, nil) })
		wg.Go(func() { cbPixels, newCb = rfxDecodeComponentProgressive(cbData, qCb, nil) })
		wg.Go(func() { crPixels, newCr = rfxDecodeComponentProgressive(crData, qCr, nil) })
		wg.Wait()
	} else {
		yPixels, newY = rfxDecodeComponentProgressive(yData, qY, nil)
		cbPixels, newCb = rfxDecodeComponentProgressive(cbData, qCb, nil)
		crPixels, newCr = rfxDecodeComponentProgressive(crData, qCr, nil)
	}

	rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)

	tileKey := uint32(yIdx)<<16 | uint32(xIdx)
	d.mu.Lock()
	d.tileCache[tileKey] = &rfxTileCoeffs{y: newY, cb: newCb, cr: newCr}
	d.mu.Unlock()

	coeffPool.Put((*coeffArr)(yPixels))
	coeffPool.Put((*coeffArr)(cbPixels))
	coeffPool.Put((*coeffArr)(crPixels))
}

// decodeTileUpgrade handles PROGRESSIVE_WBT_TILE_UPGRADE_FIRST (0xCCC7).
// The upgrade data contains RLGR-encoded delta coefficients that are added to
// the raw coefficients cached from the preceding TILE_FIRST (or TILE_SIMPLE)
// pass.  If no cached state exists for the tile (e.g. the session started
// mid-stream), the delta is decoded as if it were a standalone tile so that
// at least some image is rendered rather than nothing.
func (d *rfxProgressiveDecoder) decodeTileUpgrade(data []byte, quants []rfxQuant, output []byte, outW, outH int, parallelComponents bool) {
	if len(data) < 17 {
		return
	}

	quantIdxY := data[0]
	quantIdxCb := data[1]
	quantIdxCr := data[2]
	xIdx := binary.LittleEndian.Uint16(data[3:])
	yIdx := binary.LittleEndian.Uint16(data[5:])
	// flags := data[7]
	// quality := data[8]
	yLen := binary.LittleEndian.Uint16(data[9:])
	cbLen := binary.LittleEndian.Uint16(data[11:])
	crLen := binary.LittleEndian.Uint16(data[13:])
	// tailLen := binary.LittleEndian.Uint16(data[15:])

	off := 17
	yData := safeSlice(data, off, int(yLen))
	off += int(yLen)
	cbData := safeSlice(data, off, int(cbLen))
	off += int(cbLen)
	crData := safeSlice(data, off, int(crLen))

	qY := rfxGetQuant(quants, int(quantIdxY))
	qCb := rfxGetQuant(quants, int(quantIdxCb))
	qCr := rfxGetQuant(quants, int(quantIdxCr))

	tileKey := uint32(yIdx)<<16 | uint32(xIdx)
	d.mu.Lock()
	cached := d.tileCache[tileKey]
	d.mu.Unlock()

	var prevY, prevCb, prevCr []int16
	if cached != nil {
		prevY, prevCb, prevCr = cached.y, cached.cb, cached.cr
	}

	var yPixels, cbPixels, crPixels []int16
	var newY, newCb, newCr []int16
	if parallelComponents {
		var wg sync.WaitGroup
		wg.Go(func() { yPixels, newY = rfxDecodeComponentProgressive(yData, qY, prevY) })
		wg.Go(func() { cbPixels, newCb = rfxDecodeComponentProgressive(cbData, qCb, prevCb) })
		wg.Go(func() { crPixels, newCr = rfxDecodeComponentProgressive(crData, qCr, prevCr) })
		wg.Wait()
	} else {
		yPixels, newY = rfxDecodeComponentProgressive(yData, qY, prevY)
		cbPixels, newCb = rfxDecodeComponentProgressive(cbData, qCb, prevCb)
		crPixels, newCr = rfxDecodeComponentProgressive(crData, qCr, prevCr)
	}

	rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)

	d.mu.Lock()
	d.tileCache[tileKey] = &rfxTileCoeffs{y: newY, cb: newCb, cr: newCr}
	d.mu.Unlock()

	coeffPool.Put((*coeffArr)(yPixels))
	coeffPool.Put((*coeffArr)(cbPixels))
	coeffPool.Put((*coeffArr)(crPixels))
}

func rfxGetQuant(quants []rfxQuant, idx int) rfxQuant {
	if idx < len(quants) {
		return quants[idx]
	}
	return rfxQuant{6, 6, 6, 6, 6, 6, 6, 6, 6, 6}
}

func safeSlice(data []byte, offset, length int) []byte {
	if length <= 0 || offset < 0 || offset+length > len(data) {
		return nil
	}
	return data[offset : offset+length]
}

// rfxDecodeComponentProgressive decodes one color component for a progressive
// tile pass.  It always uses RLGR mode 1 (required by the progressive codec).
//
// If prevRaw is non-nil, it contains the raw RLGR-decoded coefficients from
// the preceding TILE_FIRST/TILE_SIMPLE pass; the newly decoded values are
// treated as a delta and added to prevRaw before processing.  Pass nil for
// the first pass (TILE_FIRST / TILE_SIMPLE).
//
// Returns:
//   - pixels: the fully decoded tile coefficients (pooled; caller must return
//     via coeffPool.Put((*coeffArr)(pixels)) when done).
//   - newRaw: the combined raw coefficients before LL3 differential decode and
//     dequantization; the caller should cache this for future upgrade passes.
//     newRaw is heap-allocated and owned by the caller.
func rfxDecodeComponentProgressive(data []byte, quant rfxQuant, prevRaw []int16) (pixels []int16, newRaw []int16) {
	const tilePixels = rfxTileSize * rfxTileSize

	arr := coeffPool.Get().(*coeffArr)
	work := arr[:]

	if data == nil {
		clear(work)
	} else {
		// Progressive codec always uses RLGR mode 1.
		work = rlgr1Decode(data, tilePixels, work)
	}

	// Add delta from the previous quality pass when upgrading.
	if prevRaw != nil {
		for i := range tilePixels {
			work[i] += prevRaw[i]
		}
	}

	// Cache the combined raw coefficients before any differential decode or
	// dequantization so a future upgrade pass can add its own delta on top.
	newRaw = make([]int16, tilePixels)
	copy(newRaw, work)

	// Apply LL3 differential decode and dequantize LL3 in a single pass
	// (identical to rfxDecodeComponent step 2).
	if quant.LL3 > 1 {
		shift := quant.LL3 - 1
		work[4032] <<= shift
		for i := 4033; i < 4096; i++ {
			work[i] = work[i-1] + work[i]<<shift
		}
	} else {
		for i := 4033; i < 4096; i++ {
			work[i] += work[i-1]
		}
	}

	rfxDequantizeSkipLL3(work, quant)
	rfxInverseDWT2D(work)

	return work, newRaw
}

// rfxDecodeComponent decodes one color component (Y, Cb, or Cr) for a 64×64 tile.
// The returned slice is backed by a *coeffArr from coeffPool; the caller must
// return it via coeffPool.Put((*coeffArr)(result)) when done.
func rfxDecodeComponent(data []byte, quant rfxQuant, rlgrMode int) []int16 {
	const tilePixels = rfxTileSize * rfxTileSize // 4096

	// Get a pooled coefficient buffer. The pool stores *coeffArr (pointer to a
	// fixed-size array) so the any interface stores a single pointer word with no
	// heap-boxing allocation.
	arr := coeffPool.Get().(*coeffArr)
	coeffs := arr[:]

	if data == nil {
		clear(coeffs)
		return coeffs
	}

	// 1. RLGR entropy decode → 4096 coefficients
	if rlgrMode == 3 {
		coeffs = rlgr3Decode(data, tilePixels, coeffs)
	} else {
		coeffs = rlgr1Decode(data, tilePixels, coeffs)
	}

	// 2. Differential decode LL3 and dequantize LL3 in a single pass.
	// Mathematical identity: cumsum(x) * 2^s == cumsum_of(x * 2^s)
	// so we can left-shift each element before accumulating.
	if quant.LL3 > 1 {
		shift := quant.LL3 - 1
		coeffs[4032] <<= shift
		for i := 4033; i < 4096; i++ {
			coeffs[i] = coeffs[i-1] + coeffs[i]<<shift
		}
	} else {
		for i := 4033; i < 4096; i++ {
			coeffs[i] += coeffs[i-1]
		}
	}

	// 3. Dequantize all subbands except LL3 (handled above)
	rfxDequantizeSkipLL3(coeffs, quant)

	// 4. Inverse DWT (3 levels)
	rfxInverseDWT2D(coeffs)

	return coeffs
}

// rfxDequantizeSkipLL3 applies dequantization per subband, skipping LL3
// (which is handled together with differential decode in rfxDecodeComponent).
func rfxDequantizeSkipLL3(coeffs []int16, q rfxQuant) {
	rfxShiftSubband(coeffs[0:1024], q.HL1)    // HL1
	rfxShiftSubband(coeffs[1024:2048], q.LH1) // LH1
	rfxShiftSubband(coeffs[2048:3072], q.HH1) // HH1
	rfxShiftSubband(coeffs[3072:3328], q.HL2) // HL2
	rfxShiftSubband(coeffs[3328:3584], q.LH2) // LH2
	rfxShiftSubband(coeffs[3584:3840], q.HH2) // HH2
	rfxShiftSubband(coeffs[3840:3904], q.HL3) // HL3
	rfxShiftSubband(coeffs[3904:3968], q.LH3) // LH3
	rfxShiftSubband(coeffs[3968:4032], q.HH3) // HH3
}

func rfxShiftSubband(data []int16, factor uint8) {
	if factor <= 1 {
		return
	}
	shift := factor - 1
	for i := range data {
		data[i] <<= shift
	}
}

// rfxInverseDWT2D performs 3-level inverse 2D discrete wavelet transform in-place.
// Buffer layout: [HL1(1024)|LH1(1024)|HH1(1024)|HL2(256)|LH2(256)|HH2(256)|HL3(64)|LH3(64)|HH3(64)|LL3(64)]
// A single temporary buffer is obtained from the pool and reused across all three
// levels, reducing pool pressure from 9 Get/Put calls (3 levels × 3 components) to 3.
func rfxInverseDWT2D(coeffs []int16) {
	bufs := idwtBufPool.Get().(*idwtBufs)
	// Level 3: 8×8 subbands → 16×16 output  (needs 16×16 = 256 elements)
	rfxIDWT2DLevel(coeffs[3840:], bufs.tmp[:256], 8)
	// Level 2: 16×16 subbands → 32×32 output (needs 32×32 = 1024 elements)
	rfxIDWT2DLevel(coeffs[3072:], bufs.tmp[:1024], 16)
	// Level 1: 32×32 subbands → 64×64 output (needs 64×64 = 4096 elements)
	rfxIDWT2DLevel(coeffs[0:], bufs.tmp[:4096], 32)
	idwtBufPool.Put(bufs)
}

// rfxIDWT2DLevel performs one level of inverse 2D DWT.
// buf contains [HL(n²)|LH(n²)|HH(n²)|LL(n²)] and is replaced with the (2n)×(2n) result.
// tmp is a caller-supplied scratch buffer of length (2n)² (must be ≥ 4n² elements).
// Uses the MS-RDPRFX lifting scheme. Order: horizontal IDWT first, then vertical.
func rfxIDWT2DLevel(buf, tmp []int16, n int) {
	nn := n * n
	size := 2 * n

	// Read subbands directly from buf — no copy needed because the horizontal
	// pass only reads from them and writes exclusively to tmp.
	hl := buf[0:nn]
	lh := buf[nn : 2*nn]
	hh := buf[2*nn : 3*nn]
	ll := buf[3*nn : 4*nn]

	// Step 1: Horizontal IDWT on each row
	for row := range n {
		rowOff := row * n
		lDstOff := row * size
		hDstOff := (row + n) * size

		tmp[lDstOff] = ll[rowOff] - int16((int32(hl[rowOff])+int32(hl[rowOff])+1)>>1)
		tmp[hDstOff] = lh[rowOff] - int16((int32(hh[rowOff])+int32(hh[rowOff])+1)>>1)

		for col := 1; col < n; col++ {
			x := col << 1
			tmp[lDstOff+x] = ll[rowOff+col] - int16((int32(hl[rowOff+col-1])+int32(hl[rowOff+col])+1)>>1)
			tmp[hDstOff+x] = lh[rowOff+col] - int16((int32(hh[rowOff+col-1])+int32(hh[rowOff+col])+1)>>1)
		}

		for col := 0; col < n-1; col++ {
			x := col << 1
			ld := (int32(hl[rowOff+col]) << 1) + ((int32(tmp[lDstOff+x]) + int32(tmp[lDstOff+x+2])) >> 1)
			hd := (int32(hh[rowOff+col]) << 1) + ((int32(tmp[hDstOff+x]) + int32(tmp[hDstOff+x+2])) >> 1)
			tmp[lDstOff+x+1] = int16(ld)
			tmp[hDstOff+x+1] = int16(hd)
		}
		x := (n - 1) << 1
		ld := (int32(hl[rowOff+n-1]) << 1) + int32(tmp[lDstOff+x])
		hd := (int32(hh[rowOff+n-1]) << 1) + int32(tmp[hDstOff+x])
		tmp[lDstOff+x+1] = int16(ld)
		tmp[hDstOff+x+1] = int16(hd)
	}

	// Step 2: Vertical IDWT on each column.
	// Process 8 columns at a time to improve cache utilisation — a cache line
	// holds 32 int16 values; 8 columns keeps the working set within one or two
	// lines per row access. All valid sizes (16, 32, 64) divide evenly by 8,
	// so the scalar tail loop is never reached in practice.
	const blk = 8
	col := 0
	for ; col+blk <= size; col += blk {
		for b := range blk {
			c := col + b
			lVal := int32(tmp[c])
			hVal := int32(tmp[n*size+c])
			buf[c] = int16(lVal - ((hVal*2 + 1) >> 1))
		}
		for row := 1; row < n; row++ {
			for b := range blk {
				c := col + b
				lIdx := row*size + c
				hIdx := (row+n)*size + c
				hPrevIdx := (row-1+n)*size + c

				even := int32(tmp[lIdx]) - ((int32(tmp[hPrevIdx]) + int32(tmp[hIdx]) + 1) >> 1)
				buf[2*row*size+c] = int16(even)

				prevEven := int32(buf[(2*row-2)*size+c])
				odd := (int32(tmp[hPrevIdx]) << 1) + ((prevEven + even) >> 1)
				buf[(2*row-1)*size+c] = int16(odd)
			}
		}
		for b := range blk {
			c := col + b
			lastEven := int32(buf[(2*n-2)*size+c])
			lastH := int32(tmp[(2*n-1)*size+c])
			buf[(2*n-1)*size+c] = int16((lastH << 1) + lastEven)
		}
	}
	for ; col < size; col++ {
		lVal := int32(tmp[col])
		hVal := int32(tmp[n*size+col])
		buf[col] = int16(lVal - ((hVal*2 + 1) >> 1))

		for row := 1; row < n; row++ {
			lIdx := row*size + col
			hIdx := (row+n)*size + col
			hPrevIdx := (row-1+n)*size + col

			even := int32(tmp[lIdx]) - ((int32(tmp[hPrevIdx]) + int32(tmp[hIdx]) + 1) >> 1)
			buf[2*row*size+col] = int16(even)

			prevEven := int32(buf[(2*row-2)*size+col])
			odd := (int32(tmp[hPrevIdx]) << 1) + ((prevEven + even) >> 1)
			buf[(2*row-1)*size+col] = int16(odd)
		}

		lastEven := int32(buf[(2*n-2)*size+col])
		lastH := int32(tmp[(2*n-1)*size+col])
		buf[(2*n-1)*size+col] = int16((lastH << 1) + lastEven)
	}
}

// rfxPlaceTile converts YCbCr tile to BGRA using tile-grid indices (xIdx, yIdx).
func rfxPlaceTile(yCoeffs, cbCoeffs, crCoeffs []int16, xIdx, yIdx int, output []byte, outW, outH int) {
	rfxPlaceTileAbs(yCoeffs, cbCoeffs, crCoeffs, xIdx*rfxTileSize, yIdx*rfxTileSize, output, outW, outH)
}

// rfxPlaceTileAbs converts YCbCr tile to BGRA and writes into the output buffer
// at absolute pixel coordinates (tileX, tileY).
// Uses ICT (Irreversible Color Transform) from MS-RDPRFX.
func rfxPlaceTileAbs(yCoeffs, cbCoeffs, crCoeffs []int16, tileX, tileY int, output []byte, outW, outH int) {
	tileW := rfxTileSize
	tileH := rfxTileSize
	if tileX+tileW > outW {
		tileW = outW - tileX
	}
	if tileY+tileH > outH {
		tileH = outH - tileY
	}
	if tileW <= 0 || tileH <= 0 {
		return
	}

	for row := 0; row < tileH; row++ {
		dstStart := ((tileY+row)*outW + tileX) * 4
		dstEnd := dstStart + tileW*4
		if dstStart < 0 || dstEnd > len(output) {
			continue
		}
		dstRow := output[dstStart:dstEnd:dstEnd]
		srcOff := row * rfxTileSize
		ictToBGRA(
			yCoeffs[srcOff:srcOff+tileW:srcOff+tileW],
			cbCoeffs[srcOff:srcOff+tileW:srcOff+tileW],
			crCoeffs[srcOff:srcOff+tileW:srcOff+tileW],
			dstRow, tileW,
		)
	}
}
