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

type rfxProgQuant struct {
	quality uint8
	yQuant  rfxQuant
	cbQuant rfxQuant
	crQuant rfxQuant
}

// rfxTileCoeffs holds the raw RLGR-decoded coefficients for one tile (all three
// components), stored before LL3 differential decode and dequantization.  This
// state is required to apply TILE_UPGRADE_FIRST delta data on top of a previous
// TILE_FIRST (or TILE_SIMPLE) pass.
//
// The fields use *coeffArr (pooled) rather than []int16 to eliminate a
// heap allocation per tile per frame.  Callers must return these to
// coeffPool when replacing or discarding a cache entry.
type rfxTileCoeffs struct {
	y, cb, cr                   *coeffArr // dequantized DWT coefficients
	signY, signCb, signCr       *coeffArr // raw RLGR coefficients (sign state for SRL upgrade)
	yBitPos, cbBitPos, crBitPos rfxQuant  // current bit positions (quant + progQuant)
	quantY, quantCb, quantCr    rfxQuant
}

type rfxProgTileWork struct {
	tileType uint16
	data     []byte
}

type rfxProgressiveDecoder struct {
	mu            sync.RWMutex
	tileCache     map[uint32]*rfxTileCoeffs // key: yIdx<<16 | xIdx
	rectsBuf      []rfxRect
	quantsBuf     []rfxQuant
	progQuantsBuf []rfxProgQuant
	tilesBuf      []rfxProgTileWork
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
	old := d.tileCache
	d.tileCache = make(map[uint32]*rfxTileCoeffs)
	d.mu.Unlock()
	// Return all cached coefficient arrays to the pool.
	for _, tc := range old {
		if tc != nil {
			if tc.y != nil {
				coeffPool.Put(tc.y)
			}
			if tc.cb != nil {
				coeffPool.Put(tc.cb)
			}
			if tc.cr != nil {
				coeffPool.Put(tc.cr)
			}
			if tc.signY != nil {
				coeffPool.Put(tc.signY)
			}
			if tc.signCb != nil {
				coeffPool.Put(tc.signCb)
			}
			if tc.signCr != nil {
				coeffPool.Put(tc.signCr)
			}
		}
	}
}

// DeleteContext is called on RDPGFX_DELETE_ENCODING_CONTEXT_PDU (MS-RDPEGFX 2.2.2.3).
// Note: Progressive tile coefficient state belongs to the surface and must NOT
// be discarded per encoding context deletion (FreeRDP also preserves surface tiles).
func (d *rfxProgressiveDecoder) DeleteContext(ctxId uint32) {
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
	regionFlags := data[5]
	extrapolate := (regionFlags & 0x01) != 0
	numTiles := binary.LittleEndian.Uint16(data[6:])
	// tileDataSize := binary.LittleEndian.Uint32(data[8:])

	offset := 12

	// Parse rects (8 bytes each: x, y, width, height as uint16, TS_RFX_RECT MS-RDPRFX 2.2.2.1.6)
	if cap(d.rectsBuf) >= int(numRects) {
		d.rectsBuf = d.rectsBuf[:numRects]
	} else {
		d.rectsBuf = make([]rfxRect, numRects)
	}
	rects := d.rectsBuf
	for i := range numRects {
		if offset+8 > len(data) {
			return nil, nil
		}
		x := int(binary.LittleEndian.Uint16(data[offset:]))
		y := int(binary.LittleEndian.Uint16(data[offset+2:]))
		w := int(binary.LittleEndian.Uint16(data[offset+4:]))
		h := int(binary.LittleEndian.Uint16(data[offset+6:]))
		rects[i] = rfxRect{x: x, y: y, w: w, h: h}
		offset += 8
	}

	// Parse quant values (5 bytes each)
	if cap(d.quantsBuf) >= int(numQuant) {
		d.quantsBuf = d.quantsBuf[:numQuant]
	} else {
		d.quantsBuf = make([]rfxQuant, numQuant)
	}
	quants := d.quantsBuf
	for i := range numQuant {
		if offset+5 > len(data) {
			return nil, nil
		}
		quants[i] = parseRfxQuant(data[offset:])
		offset += 5
	}

	// Parse progressive quant values (RFX_PROGRESSIVE_CODEC_QUANT, 16 bytes each)
	if cap(d.progQuantsBuf) >= int(numProgQuant) {
		d.progQuantsBuf = d.progQuantsBuf[:numProgQuant]
	} else {
		d.progQuantsBuf = make([]rfxProgQuant, numProgQuant)
	}
	progQuants := d.progQuantsBuf
	for i := range numProgQuant {
		if offset+16 > len(data) {
			return nil, nil
		}
		progQuants[i] = parseRfxProgQuant(data[offset:])
		offset += 16
	}

	slog.Debug("RFX progressive region", "numRects", numRects, "numQuant", numQuant, "numProgQuant", numProgQuant, "numTiles", numTiles, "quants", quants, "progQuants", progQuants)

	// Collect all decodable tiles before dispatching, so we can parallelise
	// when there are enough to amortise goroutine overhead (same threshold as
	// non-progressive decodeTileset in rfx.go).
	if cap(d.tilesBuf) >= int(numTiles) {
		d.tilesBuf = d.tilesBuf[:0]
	} else {
		d.tilesBuf = make([]rfxProgTileWork, 0, numTiles)
	}
	tiles := d.tilesBuf
	for offset+6 <= len(data) {
		tileType := binary.LittleEndian.Uint16(data[offset:])
		tileLen := binary.LittleEndian.Uint32(data[offset+2:])
		if tileLen < 6 || offset+int(tileLen) > len(data) {
			break
		}
		switch tileType {
		case progWBTTileSimple, progWBTTileFirst, progWBTTileUpgrade:
			tiles = append(tiles, rfxProgTileWork{tileType: tileType, data: data[offset+6 : offset+int(tileLen)]})
		default:
			slog.Debug("RFX: unknown progressive tile type", "type", tileType)
		}
		offset += int(tileLen)
	}
	d.tilesBuf = tiles

	const parallelTileThreshold = 12
	decodeTile := func(tw rfxProgTileWork, parallel bool) {
		switch tw.tileType {
		case progWBTTileSimple:
			d.decodeTileSimple(tw.data, quants, progQuants, surfData, outW, outH, parallel, extrapolate)
		case progWBTTileFirst:
			d.decodeTileFirst(tw.data, quants, progQuants, surfData, outW, outH, parallel, extrapolate)
		case progWBTTileUpgrade:
			d.decodeTileUpgrade(tw.data, quants, progQuants, surfData, outW, outH, parallel, extrapolate)
		}
	}
	if len(tiles) >= parallelTileThreshold {
		workers := min(runtime.NumCPU(), len(tiles))
		ch := make(chan rfxProgTileWork, len(tiles))
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

	if len(rects) == 0 && len(tiles) > 0 {
		for _, tw := range tiles {
			if len(tw.data) >= 7 {
				xIdx := int(binary.LittleEndian.Uint16(tw.data[3:]))
				yIdx := int(binary.LittleEndian.Uint16(tw.data[5:]))
				x := xIdx * 64
				y := yIdx * 64
				w := 64
				h := 64
				if x+w > outW {
					w = outW - x
				}
				if y+h > outH {
					h = outH - y
				}
				if w > 0 && h > 0 {
					rects = append(rects, rfxRect{x: x, y: y, w: w, h: h})
				}
			}
		}
	}

	return rects, quants
}

func parseRfxQuant(data []byte) rfxQuant {
	return rfxQuant{
		LL3: data[0] & 0x0F,
		HL3: data[0] >> 4,
		LH3: data[1] & 0x0F,
		HH3: data[1] >> 4,
		HL2: data[2] & 0x0F,
		LH2: data[2] >> 4,
		HH2: data[3] & 0x0F,
		HL1: data[3] >> 4,
		LH1: data[4] & 0x0F,
		HH1: data[4] >> 4,
	}
}

func parseRfxProgQuant(data []byte) rfxProgQuant {
	return rfxProgQuant{
		quality: data[0],
		yQuant:  parseRfxQuant(data[1:6]),
		cbQuant: parseRfxQuant(data[6:11]),
		crQuant: parseRfxQuant(data[11:16]),
	}
}

func rfxProgQuantShift(q rfxQuant, pq rfxQuant, hasPq bool) rfxQuant {
	if !hasPq {
		return rfxQuant{
			LL3: rfxShiftVal(q.LL3),
			LH3: rfxShiftVal(q.LH3),
			HL3: rfxShiftVal(q.HL3),
			HH3: rfxShiftVal(q.HH3),
			LH2: rfxShiftVal(q.LH2),
			HL2: rfxShiftVal(q.HL2),
			HH2: rfxShiftVal(q.HH2),
			LH1: rfxShiftVal(q.LH1),
			HL1: rfxShiftVal(q.HL1),
			HH1: rfxShiftVal(q.HH1),
		}
	}
	return rfxQuant{
		LL3: rfxShiftVal(q.LL3 + pq.LL3),
		LH3: rfxShiftVal(q.LH3 + pq.LH3),
		HL3: rfxShiftVal(q.HL3 + pq.HL3),
		HH3: rfxShiftVal(q.HH3 + pq.HH3),
		LH2: rfxShiftVal(q.LH2 + pq.LH2),
		HL2: rfxShiftVal(q.HL2 + pq.HL2),
		HH2: rfxShiftVal(q.HH2 + pq.HH2),
		LH1: rfxShiftVal(q.LH1 + pq.LH1),
		HL1: rfxShiftVal(q.HL1 + pq.HL1),
		HH1: rfxShiftVal(q.HH1 + pq.HH1),
	}
}

func rfxShiftVal(v uint8) uint8 {
	if v > 0 {
		return v - 1
	}
	return 0
}

func rfxQuantAdd(q1, q2 rfxQuant) rfxQuant {
	return rfxQuant{
		LL3: q1.LL3 + q2.LL3,
		HL3: q1.HL3 + q2.HL3,
		LH3: q1.LH3 + q2.LH3,
		HH3: q1.HH3 + q2.HH3,
		HL2: q1.HL2 + q2.HL2,
		LH2: q1.LH2 + q2.LH2,
		HH2: q1.HH2 + q2.HH2,
		HL1: q1.HL1 + q2.HL1,
		LH1: q1.LH1 + q2.LH1,
		HH1: q1.HH1 + q2.HH1,
	}
}

func rfxQuantSub(q1, q2 rfxQuant) rfxQuant {
	sub := func(a, b uint8) uint8 {
		if a >= b {
			return a - b
		}
		return 0
	}
	return rfxQuant{
		LL3: sub(q1.LL3, q2.LL3),
		HL3: sub(q1.HL3, q2.HL3),
		LH3: sub(q1.LH3, q2.LH3),
		HH3: sub(q1.HH3, q2.HH3),
		HL2: sub(q1.HL2, q2.HL2),
		LH2: sub(q1.LH2, q2.LH2),
		HH2: sub(q1.HH2, q2.HH2),
		HL1: sub(q1.HL1, q2.HL1),
		LH1: sub(q1.LH1, q2.LH1),
		HH1: sub(q1.HH1, q2.HH1),
	}
}

// decodeTileSimple handles PROGRESSIVE_WBT_TILE_SIMPLE (0xCCC5).
func (d *rfxProgressiveDecoder) decodeTileSimple(data []byte, quants []rfxQuant, progQuants []rfxProgQuant, output []byte, outW, outH int, parallelComponents, extrapolate bool) {
	if len(data) < 16 {
		return
	}

	quantIdxY := data[0]
	quantIdxCb := data[1]
	quantIdxCr := data[2]
	xIdx := binary.LittleEndian.Uint16(data[3:])
	yIdx := binary.LittleEndian.Uint16(data[5:])
	flags := data[7]
	yLen := binary.LittleEndian.Uint16(data[8:])
	cbLen := binary.LittleEndian.Uint16(data[10:])
	crLen := binary.LittleEndian.Uint16(data[12:])

	isDiff := (flags & 0x01) != 0

	off := 16
	yData := safeSlice(data, off, int(yLen))
	off += int(yLen)
	cbData := safeSlice(data, off, int(cbLen))
	off += int(cbLen)
	crData := safeSlice(data, off, int(crLen))

	qY := rfxGetQuant(quants, int(quantIdxY))
	qCb := rfxGetQuant(quants, int(quantIdxCb))
	qCr := rfxGetQuant(quants, int(quantIdxCr))

	shiftY := rfxProgQuantShift(qY, rfxQuant{}, false)
	shiftCb := rfxProgQuantShift(qCb, rfxQuant{}, false)
	shiftCr := rfxProgQuantShift(qCr, rfxQuant{}, false)

	tileKey := uint32(yIdx)<<16 | uint32(xIdx)
	var prevY, prevCb, prevCr *coeffArr
	if isDiff {
		d.mu.RLock()
		cached := d.tileCache[tileKey]
		if cached != nil {
			prevY, prevCb, prevCr = cached.y, cached.cb, cached.cr
		}
		d.mu.RUnlock()
	}

	var yPixels, cbPixels, crPixels []int16
	var newY, newCb, newCr *coeffArr
	var signY, signCb, signCr *coeffArr
	if parallelComponents {
		var wg sync.WaitGroup
		wg.Go(func() { yPixels, newY, signY = rfxDecodeComponentProgressive(yData, shiftY, prevY, isDiff, extrapolate) })
		wg.Go(func() { cbPixels, newCb, signCb = rfxDecodeComponentProgressive(cbData, shiftCb, prevCb, isDiff, extrapolate) })
		wg.Go(func() { crPixels, newCr, signCr = rfxDecodeComponentProgressive(crData, shiftCr, prevCr, isDiff, extrapolate) })
		wg.Wait()
	} else {
		yPixels, newY, signY = rfxDecodeComponentProgressive(yData, shiftY, prevY, isDiff, extrapolate)
		cbPixels, newCb, signCb = rfxDecodeComponentProgressive(cbData, shiftCb, prevCb, isDiff, extrapolate)
		crPixels, newCr, signCr = rfxDecodeComponentProgressive(crData, shiftCr, prevCr, isDiff, extrapolate)
	}

	rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)

	d.mu.Lock()
	old := d.tileCache[tileKey]
	d.tileCache[tileKey] = &rfxTileCoeffs{
		y: newY, cb: newCb, cr: newCr,
		signY: signY, signCb: signCb, signCr: signCr,
		yBitPos: qY, cbBitPos: qCb, crBitPos: qCr,
		quantY: qY, quantCb: qCb, quantCr: qCr,
	}
	d.mu.Unlock()
	if old != nil {
		if old.y != nil { coeffPool.Put(old.y) }
		if old.cb != nil { coeffPool.Put(old.cb) }
		if old.cr != nil { coeffPool.Put(old.cr) }
		if old.signY != nil { coeffPool.Put(old.signY) }
		if old.signCb != nil { coeffPool.Put(old.signCb) }
		if old.signCr != nil { coeffPool.Put(old.signCr) }
	}

	coeffPool.Put((*coeffArr)(yPixels))
	coeffPool.Put((*coeffArr)(cbPixels))
	coeffPool.Put((*coeffArr)(crPixels))
}

// decodeTileFirst handles PROGRESSIVE_WBT_TILE_FIRST (0xCCC6).
func (d *rfxProgressiveDecoder) decodeTileFirst(data []byte, quants []rfxQuant, progQuants []rfxProgQuant, output []byte, outW, outH int, parallelComponents, extrapolate bool) {
	if len(data) < 17 {
		return
	}

	quantIdxY := data[0]
	quantIdxCb := data[1]
	quantIdxCr := data[2]
	xIdx := binary.LittleEndian.Uint16(data[3:])
	yIdx := binary.LittleEndian.Uint16(data[5:])
	flags := data[7]
	quality := data[8]
	yLen := binary.LittleEndian.Uint16(data[9:])
	cbLen := binary.LittleEndian.Uint16(data[11:])
	crLen := binary.LittleEndian.Uint16(data[13:])

	isDiff := (flags & 0x01) != 0

	off := 17
	yData := safeSlice(data, off, int(yLen))
	off += int(yLen)
	cbData := safeSlice(data, off, int(cbLen))
	off += int(cbLen)
	crData := safeSlice(data, off, int(crLen))

	qY := rfxGetQuant(quants, int(quantIdxY))
	qCb := rfxGetQuant(quants, int(quantIdxCb))
	qCr := rfxGetQuant(quants, int(quantIdxCr))

	var pqY, pqCb, pqCr rfxQuant
	hasPq := false
	if quality < uint8(len(progQuants)) {
		pqY = progQuants[quality].yQuant
		pqCb = progQuants[quality].cbQuant
		pqCr = progQuants[quality].crQuant
		hasPq = true
	}

	shiftY := rfxProgQuantShift(qY, pqY, hasPq)
	shiftCb := rfxProgQuantShift(qCb, pqCb, hasPq)
	shiftCr := rfxProgQuantShift(qCr, pqCr, hasPq)

	bitPosY := rfxQuantAdd(qY, pqY)
	bitPosCb := rfxQuantAdd(qCb, pqCb)
	bitPosCr := rfxQuantAdd(qCr, pqCr)

	tileKey := uint32(yIdx)<<16 | uint32(xIdx)
	var prevY, prevCb, prevCr *coeffArr
	if isDiff {
		d.mu.RLock()
		cached := d.tileCache[tileKey]
		if cached != nil {
			prevY, prevCb, prevCr = cached.y, cached.cb, cached.cr
		}
		d.mu.RUnlock()
	}

	var yPixels, cbPixels, crPixels []int16
	var newY, newCb, newCr *coeffArr
	var signY, signCb, signCr *coeffArr
	if parallelComponents {
		var wg sync.WaitGroup
		wg.Go(func() { yPixels, newY, signY = rfxDecodeComponentProgressive(yData, shiftY, prevY, isDiff, extrapolate) })
		wg.Go(func() { cbPixels, newCb, signCb = rfxDecodeComponentProgressive(cbData, shiftCb, prevCb, isDiff, extrapolate) })
		wg.Go(func() { crPixels, newCr, signCr = rfxDecodeComponentProgressive(crData, shiftCr, prevCr, isDiff, extrapolate) })
		wg.Wait()
	} else {
		yPixels, newY, signY = rfxDecodeComponentProgressive(yData, shiftY, prevY, isDiff, extrapolate)
		cbPixels, newCb, signCb = rfxDecodeComponentProgressive(cbData, shiftCb, prevCb, isDiff, extrapolate)
		crPixels, newCr, signCr = rfxDecodeComponentProgressive(crData, shiftCr, prevCr, isDiff, extrapolate)
	}

	rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)

	d.mu.Lock()
	old := d.tileCache[tileKey]
	d.tileCache[tileKey] = &rfxTileCoeffs{
		y: newY, cb: newCb, cr: newCr,
		signY: signY, signCb: signCb, signCr: signCr,
		yBitPos: bitPosY, cbBitPos: bitPosCb, crBitPos: bitPosCr,
		quantY: qY, quantCb: qCb, quantCr: qCr,
	}
	d.mu.Unlock()
	if old != nil {
		if old.y != nil { coeffPool.Put(old.y) }
		if old.cb != nil { coeffPool.Put(old.cb) }
		if old.cr != nil { coeffPool.Put(old.cr) }
		if old.signY != nil { coeffPool.Put(old.signY) }
		if old.signCb != nil { coeffPool.Put(old.signCb) }
		if old.signCr != nil { coeffPool.Put(old.signCr) }
	}

	coeffPool.Put((*coeffArr)(yPixels))
	coeffPool.Put((*coeffArr)(cbPixels))
	coeffPool.Put((*coeffArr)(crPixels))
}

// decodeTileUpgrade handles PROGRESSIVE_WBT_TILE_UPGRADE (0xCCC7).
// MS-RDPEGFX 2.2.4.2.1.5.4
func (d *rfxProgressiveDecoder) decodeTileUpgrade(data []byte, quants []rfxQuant, progQuants []rfxProgQuant, output []byte, outW, outH int, parallelComponents, extrapolate bool) {
	if len(data) < 20 {
		return
	}

	quantIdxY := data[0]
	quantIdxCb := data[1]
	quantIdxCr := data[2]
	xIdx := binary.LittleEndian.Uint16(data[3:])
	yIdx := binary.LittleEndian.Uint16(data[5:])
	quality := data[7]
	ySrlLen := binary.LittleEndian.Uint16(data[8:])
	yRawLen := binary.LittleEndian.Uint16(data[10:])
	cbSrlLen := binary.LittleEndian.Uint16(data[12:])
	cbRawLen := binary.LittleEndian.Uint16(data[14:])
	crSrlLen := binary.LittleEndian.Uint16(data[16:])
	crRawLen := binary.LittleEndian.Uint16(data[18:])

	off := 20
	ySrlData := safeSlice(data, off, int(ySrlLen)); off += int(ySrlLen)
	yRawData := safeSlice(data, off, int(yRawLen)); off += int(yRawLen)
	cbSrlData := safeSlice(data, off, int(cbSrlLen)); off += int(cbSrlLen)
	cbRawData := safeSlice(data, off, int(cbRawLen)); off += int(cbRawLen)
	crSrlData := safeSlice(data, off, int(crSrlLen)); off += int(crSrlLen)
	crRawData := safeSlice(data, off, int(crRawLen))

	qY := rfxGetQuant(quants, int(quantIdxY))
	qCb := rfxGetQuant(quants, int(quantIdxCb))
	qCr := rfxGetQuant(quants, int(quantIdxCr))

	var pqY, pqCb, pqCr rfxQuant
	if quality < uint8(len(progQuants)) {
		pqY = progQuants[quality].yQuant
		pqCb = progQuants[quality].cbQuant
		pqCr = progQuants[quality].crQuant
	}

	tileKey := uint32(yIdx)<<16 | uint32(xIdx)
	d.mu.RLock()
	cached := d.tileCache[tileKey]
	d.mu.RUnlock()

	if cached == nil || cached.y == nil || cached.signY == nil {
		return
	}

	newYBitPos := rfxQuantAdd(qY, pqY)
	newCbBitPos := rfxQuantAdd(qCb, pqCb)
	newCrBitPos := rfxQuantAdd(qCr, pqCr)

	numBitsY := rfxQuantSub(cached.yBitPos, newYBitPos)
	numBitsCb := rfxQuantSub(cached.cbBitPos, newCbBitPos)
	numBitsCr := rfxQuantSub(cached.crBitPos, newCrBitPos)

	shiftY := rfxProgQuantShift(newYBitPos, rfxQuant{}, false)
	shiftCb := rfxProgQuantShift(newCbBitPos, rfxQuant{}, false)
	shiftCr := rfxProgQuantShift(newCrBitPos, rfxQuant{}, false)

	cached.yBitPos = newYBitPos
	cached.cbBitPos = newCbBitPos
	cached.crBitPos = newCrBitPos

	var yPixels, cbPixels, crPixels []int16
	if parallelComponents {
		var wg sync.WaitGroup
		wg.Go(func() { yPixels = rfxUpgradeComponentProgressive(ySrlData, yRawData, shiftY, numBitsY, cached.y, cached.signY, extrapolate) })
		wg.Go(func() { cbPixels = rfxUpgradeComponentProgressive(cbSrlData, cbRawData, shiftCb, numBitsCb, cached.cb, cached.signCb, extrapolate) })
		wg.Go(func() { crPixels = rfxUpgradeComponentProgressive(crSrlData, crRawData, shiftCr, numBitsCr, cached.cr, cached.signCr, extrapolate) })
		wg.Wait()
	} else {
		yPixels = rfxUpgradeComponentProgressive(ySrlData, yRawData, shiftY, numBitsY, cached.y, cached.signY, extrapolate)
		cbPixels = rfxUpgradeComponentProgressive(cbSrlData, cbRawData, shiftCb, numBitsCb, cached.cb, cached.signCb, extrapolate)
		crPixels = rfxUpgradeComponentProgressive(crSrlData, crRawData, shiftCr, numBitsCr, cached.cr, cached.signCr, extrapolate)
	}

	rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)

	coeffPool.Put((*coeffArr)(yPixels))
	coeffPool.Put((*coeffArr)(cbPixels))
	coeffPool.Put((*coeffArr)(crPixels))
}

// rfxBitStream provides bit-level reading from a byte slice (MSB first).
type rfxBitStream struct {
	data   []byte
	pos    int
	bits   uint64
	bitLen int
}

func newBitStream(data []byte) *rfxBitStream {
	bs := &rfxBitStream{data: data}
	bs.fill()
	return bs
}

func (bs *rfxBitStream) fill() {
	for bs.bitLen <= 56 && bs.pos < len(bs.data) {
		bs.bits |= uint64(bs.data[bs.pos]) << (56 - bs.bitLen)
		bs.pos++
		bs.bitLen += 8
	}
}

func (bs *rfxBitStream) readBit() uint32 {
	if bs.bitLen < 1 {
		bs.fill()
		if bs.bitLen < 1 {
			return 0
		}
	}
	bit := uint32(bs.bits >> 63)
	bs.bits <<= 1
	bs.bitLen--
	return bit
}

func (bs *rfxBitStream) readBits(n int) uint32 {
	if n <= 0 {
		return 0
	}
	if bs.bitLen < n {
		bs.fill()
		if bs.bitLen < n {
			n = bs.bitLen
			if n <= 0 {
				return 0
			}
		}
	}
	val := uint32(bs.bits >> (64 - n))
	bs.bits <<= n
	bs.bitLen -= n
	return val
}

type rfxSRLState struct {
	srl  *rfxBitStream
	raw  *rfxBitStream
	kp   uint32
	nz   int
	mode bool
}

func (s *rfxSRLState) readSRL(numBits uint32) int16 {
	if s.nz > 0 {
		s.nz--
		return 0
	}
	k := s.kp / 8
	if !s.mode {
		bit := s.srl.readBit()
		if bit == 0 {
			s.nz = (1 << k) - 1
			s.kp += 4
			if s.kp > 80 {
				s.kp = 80
			}
			return 0
		} else {
			s.nz = 0
			s.mode = true
			if k > 0 {
				s.nz = int(s.srl.readBits(int(k)))
			}
			if s.nz > 0 {
				s.nz--
				return 0
			}
		}
	}
	s.mode = false
	sign := s.srl.readBit()
	if s.kp < 6 {
		s.kp = 0
	} else {
		s.kp -= 6
	}
	if numBits == 1 {
		if sign != 0 {
			return -1
		}
		return 1
	}
	mag := uint32(1)
	maxVal := (uint32(1) << numBits) - 1
	for mag < maxVal {
		bit := s.srl.readBit()
		if bit != 0 {
			break
		}
		mag++
	}
	if sign != 0 {
		return -int16(mag)
	}
	return int16(mag)
}

func (s *rfxSRLState) upgradeBlock(current, sign []int16, length int, shift, numBits uint8, isLL bool) {
	if numBits == 0 || length > len(current) || length > len(sign) {
		return
	}
	if isLL {
		for i := 0; i < length; i++ {
			input := int32(s.raw.readBits(int(numBits)))
			current[i] += int16(input << shift)
		}
		return
	}
	for i := 0; i < length; i++ {
		var input int32
		if sign[i] > 0 {
			input = int32(s.raw.readBits(int(numBits)))
		} else if sign[i] < 0 {
			input = -int32(s.raw.readBits(int(numBits)))
		} else {
			srlVal := s.readSRL(uint32(numBits))
			sign[i] = srlVal
			input = int32(srlVal)
		}
		current[i] += int16(input << shift)
	}
}

func rfxUpgradeComponentProgressive(srlData, rawData []byte, shift, numBits rfxQuant, current, sign *coeffArr, extrapolate bool) []int16 {
	state := &rfxSRLState{
		srl: newBitStream(srlData),
		raw: newBitStream(rawData),
		kp:  8,
	}

	cur := (*current)[:]
	sgn := (*sign)[:]

	if !extrapolate {
		state.upgradeBlock(cur[0:1024], sgn[0:1024], 1024, shift.HL1, numBits.HL1, false)
		state.upgradeBlock(cur[1024:2048], sgn[1024:2048], 1024, shift.LH1, numBits.LH1, false)
		state.upgradeBlock(cur[2048:3072], sgn[2048:3072], 1024, shift.HH1, numBits.HH1, false)
		state.upgradeBlock(cur[3072:3328], sgn[3072:3328], 256, shift.HL2, numBits.HL2, false)
		state.upgradeBlock(cur[3328:3584], sgn[3328:3584], 256, shift.LH2, numBits.LH2, false)
		state.upgradeBlock(cur[3584:3840], sgn[3584:3840], 256, shift.HH2, numBits.HH2, false)
		state.upgradeBlock(cur[3840:3904], sgn[3840:3904], 64, shift.HL3, numBits.HL3, false)
		state.upgradeBlock(cur[3904:3968], sgn[3904:3968], 64, shift.LH3, numBits.LH3, false)
		state.upgradeBlock(cur[3968:4032], sgn[3968:4032], 64, shift.HH3, numBits.HH3, false)
		state.upgradeBlock(cur[4032:4096], sgn[4032:4096], 64, shift.LL3, numBits.LL3, true)
	} else {
		state.upgradeBlock(cur[0:1023], sgn[0:1023], 1023, shift.HL1, numBits.HL1, false)
		state.upgradeBlock(cur[1023:2046], sgn[1023:2046], 1023, shift.LH1, numBits.LH1, false)
		state.upgradeBlock(cur[2046:3007], sgn[2046:3007], 961, shift.HH1, numBits.HH1, false)
		state.upgradeBlock(cur[3007:3279], sgn[3007:3279], 272, shift.HL2, numBits.HL2, false)
		state.upgradeBlock(cur[3279:3551], sgn[3279:3551], 272, shift.LH2, numBits.LH2, false)
		state.upgradeBlock(cur[3551:3807], sgn[3551:3807], 256, shift.HH2, numBits.HH2, false)
		state.upgradeBlock(cur[3807:3879], sgn[3807:3879], 72, shift.HL3, numBits.HL3, false)
		state.upgradeBlock(cur[3879:3951], sgn[3879:3951], 72, shift.LH3, numBits.LH3, false)
		state.upgradeBlock(cur[3951:4015], sgn[3951:4015], 64, shift.HH3, numBits.HH3, false)
		state.upgradeBlock(cur[4015:4096], sgn[4015:4096], 81, shift.LL3, numBits.LL3, true)
	}

	arr := coeffPool.Get().(*coeffArr)
	work := arr[:]
	copy(work, cur)

	if !extrapolate {
		rfxInverseDWT2D(work)
	} else {
		bufs := idwtBufPool.Get().(*idwtBufs)
		rfxInverseDWTExtrapolate(work, bufs.tmp)
		idwtBufPool.Put(bufs)
	}

	return work
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
// tile pass. It always uses RLGR mode 1 (required by the progressive codec).
func rfxDecodeComponentProgressive(data []byte, shift rfxQuant, prevDequant *coeffArr, isDiff, extrapolate bool) (pixels []int16, newDequant, rawSign *coeffArr) {
	const tilePixels = rfxTileSize * rfxTileSize

	arr := coeffPool.Get().(*coeffArr)
	work := arr[:]

	rawSign = coeffPool.Get().(*coeffArr)

	if data == nil {
		clear(work)
		clear(rawSign[:])
	} else {
		// Progressive codec always uses RLGR mode 1.
		work = rlgr1Decode(data, tilePixels, work)
		copy(rawSign[:], work[:tilePixels])
	}

	if !extrapolate {
		// Differential decode LL3 (64 elements starting at offset 4032)
		for i := 4033; i < 4096; i++ {
			work[i] += work[i-1]
		}

		// Dequantize all 10 subbands with progressive shift
		rfxShiftSlice(work[0:1024], shift.HL1)
		rfxShiftSlice(work[1024:2048], shift.LH1)
		rfxShiftSlice(work[2048:3072], shift.HH1)
		rfxShiftSlice(work[3072:3328], shift.HL2)
		rfxShiftSlice(work[3328:3584], shift.LH2)
		rfxShiftSlice(work[3584:3840], shift.HH2)
		rfxShiftSlice(work[3840:3904], shift.HL3)
		rfxShiftSlice(work[3904:3968], shift.LH3)
		rfxShiftSlice(work[3968:4032], shift.HH3)
		rfxShiftSlice(work[4032:4096], shift.LL3)
	} else {
		// Differential decode LL3 band (81 elements starting at offset 4015).
		for i := 4016; i < 4096; i++ {
			work[i] += work[i-1]
		}

		// Dequantize all 10 subbands with progressive shift
		rfxShiftSlice(work[0:1023], shift.HL1)
		rfxShiftSlice(work[1023:2046], shift.LH1)
		rfxShiftSlice(work[2046:3007], shift.HH1)
		rfxShiftSlice(work[3007:3279], shift.HL2)
		rfxShiftSlice(work[3279:3551], shift.LH2)
		rfxShiftSlice(work[3551:3807], shift.HH2)
		rfxShiftSlice(work[3807:3879], shift.HL3)
		rfxShiftSlice(work[3879:3951], shift.LH3)
		rfxShiftSlice(work[3951:4015], shift.HH3)
		rfxShiftSlice(work[4015:4096], shift.LL3)
	}

	// Add differential delta from previous pass if RFX_TILE_DIFFERENCE is set
	if isDiff && prevDequant != nil {
		prev := (*prevDequant)[:]
		for i := range tilePixels {
			work[i] = clampi16(int32(work[i]) + int32(prev[i]))
		}
	}

	// Cache the dequantized coefficients for future differential / upgrade passes
	newDequant = coeffPool.Get().(*coeffArr)
	copy((*newDequant)[:], work[:tilePixels])

	if !extrapolate {
		rfxInverseDWT2D(work)
	} else {
		// Extrapolated Inverse DWT (3 levels)
		bufs := idwtBufPool.Get().(*idwtBufs)
		rfxInverseDWTExtrapolate(work, bufs.tmp)
		idwtBufPool.Put(bufs)
	}

	return work, newDequant, rawSign
}

func rfxShiftSlice(data []int16, shift uint8) {
	if shift == 0 {
		return
	}
	for i := range data {
		data[i] <<= shift
	}
}

func clampi16(val int32) int16 {
	if val < -32768 {
		return -32768
	}
	if val > 32767 {
		return 32767
	}
	return int16(val)
}

func rfxIDWTX(pLow, pHigh, pDst []int16, nLowStep, nHighStep, nDstStep, nLowCount, nHighCount, nDstCount int) {
	for i := range nDstCount {
		pL := i * nLowStep
		pH := i * nHighStep
		pX := i * nDstStep

		H0 := int32(pHigh[pH])
		pH++
		L0 := int32(pLow[pL])
		pL++

		X0 := clampi16(L0 - H0)
		X2 := X0

		for j := 0; j < nHighCount-1; j++ {
			H1 := int32(pHigh[pH])
			pH++
			L0 = int32(pLow[pL])
			pL++
			X2 = clampi16(L0 - ((H0 + H1) / 2))
			X1 := clampi16(((int32(X0) + int32(X2)) / 2) + (2 * H0))
			pDst[pX] = X0
			pDst[pX+1] = X1
			pX += 2
			X0 = X2
			H0 = H1
		}

		if nLowCount <= nHighCount+1 {
			if nLowCount <= nHighCount {
				pDst[pX] = X2
				pDst[pX+1] = clampi16(int32(X2) + 2*H0)
			} else {
				L0 = int32(pLow[pL])
				pL++
				X0 = clampi16(L0 - H0)
				pDst[pX] = X2
				pDst[pX+1] = clampi16(((int32(X0) + int32(X2)) / 2) + 2*H0)
				pDst[pX+2] = X0
			}
		} else {
			L0 = int32(pLow[pL])
			pL++
			X0 = clampi16(L0 - (H0 / 2))
			pDst[pX] = X2
			pDst[pX+1] = clampi16(((int32(X0) + int32(X2)) / 2) + 2*H0)
			pDst[pX+2] = X0
			L0 = int32(pLow[pL])
			pDst[pX+3] = clampi16((int32(X0) + L0) / 2)
		}
	}
}

func rfxIDWTY(pLow, pHigh, pDst []int16, nLowStep, nHighStep, nDstStep, nLowCount, nHighCount, nDstCount int) {
	for i := range nDstCount {
		pL := i
		pH := i
		pX := i

		H0 := int32(pHigh[pH])
		pH += nHighStep
		L0 := int32(pLow[pL])
		pL += nLowStep

		X0 := clampi16(L0 - H0)
		X2 := X0

		for j := 0; j < nHighCount-1; j++ {
			H1 := int32(pHigh[pH])
			pH += nHighStep
			L0 = int32(pLow[pL])
			pL += nLowStep
			X2 = clampi16(L0 - ((H0 + H1) / 2))
			X1 := clampi16(((int32(X0) + int32(X2)) / 2) + 2*H0)
			pDst[pX] = X0
			pX += nDstStep
			pDst[pX] = X1
			pX += nDstStep
			X0 = X2
			H0 = H1
		}

		if nLowCount <= nHighCount+1 {
			if nLowCount <= nHighCount {
				pDst[pX] = X2
				pX += nDstStep
				pDst[pX] = clampi16(int32(X2) + 2*H0)
			} else {
				L0 = int32(pLow[pL])
				pL += nLowStep
				X0 = clampi16(L0 - H0)
				pDst[pX] = X2
				pX += nDstStep
				pDst[pX] = clampi16(((int32(X0) + int32(X2)) / 2) + 2*H0)
				pX += nDstStep
				pDst[pX] = X0
			}
		} else {
			L0 = int32(pLow[pL])
			pL += nLowStep
			X0 = clampi16(L0 - (H0 / 2))
			pDst[pX] = X2
			pX += nDstStep
			pDst[pX] = clampi16(((int32(X0) + int32(X2)) / 2) + 2*H0)
			pX += nDstStep
			pDst[pX] = X0
			pX += nDstStep
			L0 = int32(pLow[pL])
			pDst[pX] = clampi16((int32(X0) + L0) / 2)
		}
	}
}

func rfxDWT2DDecodeBlock(buffer, temp []int16, level int) {
	nBandL := (64 >> level) + 1
	var nBandH int
	if level == 1 {
		nBandH = (64 >> 1) - 1
	} else {
		nBandH = (64 + (1 << (level - 1))) >> level
	}

	offset := 0
	hl := buffer[offset : offset+nBandH*nBandL]
	offset += nBandH * nBandL
	lh := buffer[offset : offset+nBandL*nBandH]
	offset += nBandL * nBandH
	hh := buffer[offset : offset+nBandH*nBandH]
	offset += nBandH * nBandH
	ll := buffer[offset : offset+nBandL*nBandL]

	nDstStep := nBandL + nBandH

	tempL := temp[0 : nBandL*nDstStep]
	tempH := temp[nBandL*nDstStep : (nBandL+nBandH)*nDstStep]

	// horizontal (LL + HL -> L)
	rfxIDWTX(ll, hl, tempL, nBandL, nBandH, nDstStep, nBandL, nBandH, nBandL)

	// horizontal (LH + HH -> H)
	rfxIDWTX(lh, hh, tempH, nBandL, nBandH, nDstStep, nBandL, nBandH, nBandH)

	// vertical (L + H -> buffer)
	rfxIDWTY(tempL, tempH, buffer[:nDstStep*nDstStep], nDstStep, nDstStep, nDstStep, nBandL, nBandH, nBandL+nBandH)
}

func rfxInverseDWTExtrapolate(buffer, temp []int16) {
	rfxDWT2DDecodeBlock(buffer[3807:], temp, 3)
	rfxDWT2DDecodeBlock(buffer[3007:], temp, 2)
	rfxDWT2DDecodeBlock(buffer[0:], temp, 1)
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

	// Step 1: Horizontal IDWT on each row (fused even+odd passes).
	// Instead of two separate loops — even pass writing to tmp, then odd pass
	// re-reading those values — we keep the last even value in a register and
	// compute the preceding odd value in the same iteration.  This eliminates
	// 2*(n-1) reads of tmp per row (one even[col-1] and one even[col] per odd
	// position), replacing them with register references.
	// Valid sizes in practice: n = 8, 16, 32.
	for row := range n {
		rowOff := row * n
		lDstOff := row * size
		hDstOff := (row + n) * size

		// col=0: even boundary (no left neighbour, hl[-1] = hl[0]).
		prevEvenL := ll[rowOff] - int16((int32(hl[rowOff])*2+1)>>1)
		prevEvenH := lh[rowOff] - int16((int32(hh[rowOff])*2+1)>>1)
		tmp[lDstOff] = prevEvenL
		tmp[hDstOff] = prevEvenH

		// col=1..n-1: compute even[col], then immediately compute odd[col-1]
		// using prevEven (=even[col-1], still in register) and the just-computed
		// even[col] — no re-read of tmp required.
		for col := 1; col < n; col++ {
			x := col << 1
			evenL := ll[rowOff+col] - int16((int32(hl[rowOff+col-1])+int32(hl[rowOff+col])+1)>>1)
			evenH := lh[rowOff+col] - int16((int32(hh[rowOff+col-1])+int32(hh[rowOff+col])+1)>>1)
			tmp[lDstOff+x-1] = int16((int32(hl[rowOff+col-1])<<1) + ((int32(prevEvenL)+int32(evenL))>>1))
			tmp[hDstOff+x-1] = int16((int32(hh[rowOff+col-1])<<1) + ((int32(prevEvenH)+int32(evenH))>>1))
			tmp[lDstOff+x] = evenL
			tmp[hDstOff+x] = evenH
			prevEvenL = evenL
			prevEvenH = evenH
		}

		// last odd[n-1]: right boundary, even[n] = even[n-1].
		x := (n - 1) << 1
		tmp[lDstOff+x+1] = int16((int32(hl[rowOff+n-1])<<1) + int32(prevEvenL))
		tmp[hDstOff+x+1] = int16((int32(hh[rowOff+n-1])<<1) + int32(prevEvenH))
	}

	// Step 2: Vertical IDWT on each column.
	// Process 8 columns at a time to improve cache utilisation — a cache line
	// holds 32 int16 values; 8 columns keeps the working set within one or two
	// lines per row access. All valid sizes (16, 32, 64) divide evenly by 8,
	// so the scalar tail loop is never reached in practice.
	const blk = 8
	col := 0
	for ; col+blk <= size; col += blk {
		// Row 0: first even output (no previous odd)
		l0 := tmp[col : col+blk]
		h0 := tmp[n*size+col : n*size+col+blk]
		out0 := buf[col : col+blk]
		for b := range blk {
			out0[b] = int16(int32(l0[b]) - ((int32(h0[b])*2 + 1) >> 1))
		}
		// Rows 1..n-1: interleaved even/odd outputs
		for row := 1; row < n; row++ {
			lBase := row*size + col
			hBase := (row+n)*size + col
			hPrevBase := (row-1+n)*size + col
			evenBase := 2*row*size + col
			prevEvenBase := (2*row-2)*size + col
			oddBase := (2*row-1)*size + col

			l := tmp[lBase : lBase+blk]
			h := tmp[hBase : hBase+blk]
			hPrev := tmp[hPrevBase : hPrevBase+blk]
			evenOut := buf[evenBase : evenBase+blk]
			prevEvenIn := buf[prevEvenBase : prevEvenBase+blk]
			oddOut := buf[oddBase : oddBase+blk]

			for b := range blk {
				hPrevV := int32(hPrev[b])
				even := int32(l[b]) - ((hPrevV + int32(h[b]) + 1) >> 1)
				evenOut[b] = int16(even)
				oddOut[b] = int16((hPrevV << 1) + ((int32(prevEvenIn[b]) + even) >> 1))
			}
		}
		// Last odd row
		lastEvenBase := (2*n-2)*size + col
		lastHBase := (2*n-1)*size + col
		lastEvenSlice := buf[lastEvenBase : lastEvenBase+blk]
		lastHSlice := tmp[lastHBase : lastHBase+blk]
		lastOddOut := buf[lastHBase : lastHBase+blk]
		for b := range blk {
			lastOddOut[b] = int16((int32(lastHSlice[b]) << 1) + int32(lastEvenSlice[b]))
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
