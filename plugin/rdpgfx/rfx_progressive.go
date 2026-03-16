package rdpgfx

// RFX Progressive Codec decoder (MS-RDPRFX / MS-RDPEGFX 2.2.4).
// Handles RDPGFX_CODECID_CAPROGRESSIVE (0x0009) in WIRE_TO_SURFACE_PDU_2.

import (
"encoding/binary"
"fmt"
"log/slog"
)

// Progressive block types
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

type rfxProgressiveDecoder struct {
debugTileCount int // limits debug logging
}

func newRfxProgressiveDecoder() *rfxProgressiveDecoder {
return &rfxProgressiveDecoder{}
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
slog.Debug(fmt.Sprintf("RFX: invalid block type=0x%04X len=%d", blockType, blockLen))
break
}

blockData := data[offset+6 : offset+int(blockLen)]

switch blockType {
case progWBTSync, progWBTFrameBegin, progWBTFrameEnd, progWBTContext:
// handled implicitly
case progWBTRegion:
rects = append(rects, d.decodeRegion(blockData, surfData, width, height)...)
default:
slog.Debug(fmt.Sprintf("RFX: unknown block 0x%04X", blockType))
}

offset += int(blockLen)
}

return rects
}

func (d *rfxProgressiveDecoder) decodeRegion(data []byte, output []byte, width, height int) []rfxRect {
if len(data) < 12 {
return nil
}

_ = data[0] // tileSize (64)
numRects := binary.LittleEndian.Uint16(data[1:])
numQuant := data[3]
numProgQuant := data[4]
flags := data[5]
numTiles := binary.LittleEndian.Uint16(data[6:])
_ = binary.LittleEndian.Uint32(data[8:]) // tileDataSize

offset := 12

// Parse rects (8 bytes each: x, y, width, height as uint16)
rects := make([]rfxRect, numRects)
for i := uint16(0); i < numRects; i++ {
if offset+8 > len(data) {
return nil
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
for i := uint8(0); i < numQuant; i++ {
if offset+5 > len(data) {
return nil
}
quants[i] = parseRfxQuant(data[offset:])
offset += 5
}

// Skip progressive quant values (16 bytes each)
offset += int(numProgQuant) * 16

if flags&0x01 != 0 {
slog.Warn("RFX: RFX_DWT_REDUCE_EXTRAPOLATE flag set (not fully supported)")
}
slog.Debug(fmt.Sprintf("RFX: region %d tiles, %d quants, %d progQuants, %d rects, flags=0x%02X",
numTiles, numQuant, numProgQuant, numRects, flags))

for i := uint16(0); i < numTiles; i++ {
if offset+6 > len(data) {
break
}
tileType := binary.LittleEndian.Uint16(data[offset:])
tileLen := binary.LittleEndian.Uint32(data[offset+2:])

if tileLen < 6 || offset+int(tileLen) > len(data) {
break
}

tileData := data[offset+6 : offset+int(tileLen)]

switch tileType {
case progWBTTileSimple:
d.decodeTileSimple(tileData, quants, output, width, height)
case progWBTTileFirst:
d.decodeTileFirst(tileData, quants, output, width, height)
case progWBTTileUpgrade:
// Progressive upgrade — skip for now (first pass is sufficient)
}

offset += int(tileLen)
}
return rects
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
func (d *rfxProgressiveDecoder) decodeTileSimple(data []byte, quants []rfxQuant, output []byte, outW, outH int) {
if len(data) < 16 {
return
}

quantIdxY := data[0]
quantIdxCb := data[1]
quantIdxCr := data[2]
xIdx := binary.LittleEndian.Uint16(data[3:])
yIdx := binary.LittleEndian.Uint16(data[5:])
_ = data[7] // flags
yLen := binary.LittleEndian.Uint16(data[8:])
cbLen := binary.LittleEndian.Uint16(data[10:])
crLen := binary.LittleEndian.Uint16(data[12:])
_ = binary.LittleEndian.Uint16(data[14:]) // tailLen

off := 16
yData := safeSlice(data, off, int(yLen))
off += int(yLen)
cbData := safeSlice(data, off, int(cbLen))
off += int(cbLen)
crData := safeSlice(data, off, int(crLen))

qY := rfxGetQuant(quants, int(quantIdxY))
qCb := rfxGetQuant(quants, int(quantIdxCb))
qCr := rfxGetQuant(quants, int(quantIdxCr))

debug := d.debugTileCount < 1
d.debugTileCount++
if debug {
slog.Debug(fmt.Sprintf("RFX: TILE_SIMPLE (%d,%d) yLen=%d cbLen=%d crLen=%d qY=%+v",
xIdx, yIdx, yLen, cbLen, crLen, qY))
}

yPixels := rfxDecodeComponent(yData, qY, debug)
cbPixels := rfxDecodeComponent(cbData, qCb, false)
crPixels := rfxDecodeComponent(crData, qCr, false)

rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)
}

// decodeTileFirst handles PROGRESSIVE_WBT_TILE_FIRST (0xCCC6).
// Same as TILE_SIMPLE but with an extra quality byte.
func (d *rfxProgressiveDecoder) decodeTileFirst(data []byte, quants []rfxQuant, output []byte, outW, outH int) {
if len(data) < 17 {
return
}

quantIdxY := data[0]
quantIdxCb := data[1]
quantIdxCr := data[2]
xIdx := binary.LittleEndian.Uint16(data[3:])
yIdx := binary.LittleEndian.Uint16(data[5:])
_ = data[7] // flags
_ = data[8] // quality (progressive)
yLen := binary.LittleEndian.Uint16(data[9:])
cbLen := binary.LittleEndian.Uint16(data[11:])
crLen := binary.LittleEndian.Uint16(data[13:])
_ = binary.LittleEndian.Uint16(data[15:]) // tailLen

off := 17
yData := safeSlice(data, off, int(yLen))
off += int(yLen)
cbData := safeSlice(data, off, int(cbLen))
off += int(cbLen)
crData := safeSlice(data, off, int(crLen))

qY := rfxGetQuant(quants, int(quantIdxY))
qCb := rfxGetQuant(quants, int(quantIdxCb))
qCr := rfxGetQuant(quants, int(quantIdxCr))

debug := d.debugTileCount < 1
d.debugTileCount++
if debug {
slog.Debug(fmt.Sprintf("RFX: TILE_FIRST (%d,%d) yLen=%d cbLen=%d crLen=%d qY=%+v quality=%d",
xIdx, yIdx, yLen, cbLen, crLen, qY, data[8]))
}

yPixels := rfxDecodeComponent(yData, qY, debug)
cbPixels := rfxDecodeComponent(cbData, qCb, false)
crPixels := rfxDecodeComponent(crData, qCr, false)

rfxPlaceTile(yPixels, cbPixels, crPixels, int(xIdx), int(yIdx), output, outW, outH)
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

// rfxDecodeComponent decodes one color component (Y, Cb, or Cr) for a 64×64 tile.
func rfxDecodeComponent(data []byte, quant rfxQuant, debug bool) []int16 {
const tilePixels = rfxTileSize * rfxTileSize // 4096

if data == nil {
return make([]int16, tilePixels)
}

// 1. RLGR1 entropy decode → 4096 coefficients
coeffs := rlgr1Decode(data, tilePixels)
if debug {
slog.Debug(fmt.Sprintf("RFX: RLGR first8: %v LL3[0..3]=%v", coeffs[:8], coeffs[4032:4036]))
}

// 2. Differential decode LL3 (positions 4032..4095)
// RLGR stores LL3 as deltas; accumulate to recover absolute values.
for i := 4033; i < 4096; i++ {
coeffs[i] += coeffs[i-1]
}

// 3. Dequantize (left-shift by quant-1 per subband)
rfxDequantize(coeffs, quant)
if debug {
slog.Debug(fmt.Sprintf("RFX: dequant LL3[0..3]=%v", coeffs[4032:4036]))
}

// 4. Inverse DWT (3 levels)
rfxInverseDWT2D(coeffs)
if debug {
slog.Debug(fmt.Sprintf("RFX: IDWT pixel[0..7]: %v", coeffs[:8]))
}

return coeffs
}

// rfxDequantize applies dequantization (left shift by factor-1) per subband.
func rfxDequantize(coeffs []int16, q rfxQuant) {
rfxShiftSubband(coeffs[0:1024], q.HL1)    // HL1
rfxShiftSubband(coeffs[1024:2048], q.LH1) // LH1
rfxShiftSubband(coeffs[2048:3072], q.HH1) // HH1
rfxShiftSubband(coeffs[3072:3328], q.HL2) // HL2
rfxShiftSubband(coeffs[3328:3584], q.LH2) // LH2
rfxShiftSubband(coeffs[3584:3840], q.HH2) // HH2
rfxShiftSubband(coeffs[3840:3904], q.HL3) // HL3
rfxShiftSubband(coeffs[3904:3968], q.LH3) // LH3
rfxShiftSubband(coeffs[3968:4032], q.HH3) // HH3
rfxShiftSubband(coeffs[4032:4096], q.LL3) // LL3
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
// Matches FreeRDP's rfx_dwt_2d_decode: horizontal IDWT first, then vertical IDWT.
func rfxInverseDWT2D(coeffs []int16) {
// Level 3: 8×8 subbands → 16×16 output (replaces [3840:4096])
rfxIDWT2DLevel(coeffs[3840:], 8)
// Level 2: 16×16 subbands → 32×32 output, LL2 = level3 output at [3840:]
rfxIDWT2DLevel(coeffs[3072:], 16)
// Level 1: 32×32 subbands → 64×64 output, LL1 = level2 output at [3072:]
rfxIDWT2DLevel(coeffs[0:], 32)
}

// rfxIDWT2DLevel performs one level of inverse 2D DWT.
// buf contains [HL(n²)|LH(n²)|HH(n²)|LL(n²)] and is replaced with the (2n)×(2n) result.
// Uses the MS-RDPRFX lifting scheme (with H-coefficient scaling).
// Order: horizontal IDWT first, then vertical IDWT (matching FreeRDP).
func rfxIDWT2DLevel(buf []int16, n int) {
nn := n * n
size := 2 * n

// Extract subbands
hl := make([]int16, nn) // HL: high horizontal, low vertical
lh := make([]int16, nn) // LH: low horizontal, high vertical
hh := make([]int16, nn) // HH: high horizontal, high vertical
ll := make([]int16, nn) // LL: low horizontal, low vertical
copy(hl, buf[0:nn])
copy(lh, buf[nn:2*nn])
copy(hh, buf[2*nn:3*nn])
copy(ll, buf[3*nn:4*nn])

// tmp buffer for intermediate results (after horizontal IDWT)
// Layout: L part (size×n rows at top), H part (size×n rows at bottom)
tmp := make([]int16, size*size)

// Step 1: Horizontal IDWT on each row
// L part: combine LL (low horizontal) and HL (high horizontal)
// H part: combine LH (low horizontal) and HH (high horizontal)
for row := 0; row < n; row++ {
rowOff := row * n
lDstOff := row * size  // L part in top half
hDstOff := (row + n) * size // H part in bottom half

// Even coefficients (undo update step)
// dst[0] = lo[0] - ((hi[0] + hi[0] + 1) >> 1) = lo[0] - ((2*hi[0] + 1) >> 1)
tmp[lDstOff] = ll[rowOff] - int16((int32(hl[rowOff])+int32(hl[rowOff])+1)>>1)
tmp[hDstOff] = lh[rowOff] - int16((int32(hh[rowOff])+int32(hh[rowOff])+1)>>1)

for col := 1; col < n; col++ {
x := col << 1
tmp[lDstOff+x] = ll[rowOff+col] - int16((int32(hl[rowOff+col-1])+int32(hl[rowOff+col])+1)>>1)
tmp[hDstOff+x] = lh[rowOff+col] - int16((int32(hh[rowOff+col-1])+int32(hh[rowOff+col])+1)>>1)
}

// Odd coefficients (undo predict step, with H<<1 scaling)
for col := 0; col < n-1; col++ {
x := col << 1
ld := (int32(hl[rowOff+col]) << 1) + ((int32(tmp[lDstOff+x]) + int32(tmp[lDstOff+x+2])) >> 1)
hd := (int32(hh[rowOff+col]) << 1) + ((int32(tmp[hDstOff+x]) + int32(tmp[hDstOff+x+2])) >> 1)
tmp[lDstOff+x+1] = int16(ld)
tmp[hDstOff+x+1] = int16(hd)
}
// Last odd (boundary: eNext = eCur)
x := (n - 1) << 1
ld := (int32(hl[rowOff+n-1]) << 1) + int32(tmp[lDstOff+x])
hd := (int32(hh[rowOff+n-1]) << 1) + int32(tmp[hDstOff+x])
tmp[lDstOff+x+1] = int16(ld)
tmp[hDstOff+x+1] = int16(hd)
}

// Step 2: Vertical IDWT on each column
// L = tmp[0:n*size] (top half), H = tmp[n*size:size*size] (bottom half)
for col := 0; col < size; col++ {
// First even (boundary: hPrev = h[0])
lVal := int32(tmp[col])
hVal := int32(tmp[n*size+col])
buf[col] = int16(lVal - ((hVal*2 + 1) >> 1))

for row := 1; row < n; row++ {
lIdx := row*size + col
hIdx := (row+n)*size + col
hPrevIdx := (row-1+n)*size + col

// Even coefficient
even := int32(tmp[lIdx]) - ((int32(tmp[hPrevIdx]) + int32(tmp[hIdx]) + 1) >> 1)
buf[2*row*size+col] = int16(even)

// Odd coefficient (for row-1)
prevEven := int32(buf[(2*row-2)*size+col])
odd := (int32(tmp[hPrevIdx]) << 1) + ((prevEven + even) >> 1)
buf[(2*row-1)*size+col] = int16(odd)
}

// Last odd (boundary: eNext = eCur)
lastEven := int32(buf[(2*n-2)*size+col])
lastH := int32(tmp[(2*n-1)*size+col])
buf[(2*n-1)*size+col] = int16((lastH << 1) + lastEven)
}
}

// rfxPlaceTile converts YCbCr tile to BGRA and writes into the output buffer.
// Uses ICT (Irreversible Color Transform) from MS-RDPRFX.
// The IDWT output values are in a domain scaled by <<5 from the encode-side RGB→YCbCr.
// The +4096 = 128<<5 undoes the -128 centering applied before the forward color transform.
// Fixed-point constants (×2^16): CrR=91916, CrG=46819, CbG=22527, CbB=115992.
func rfxPlaceTile(yCoeffs, cbCoeffs, crCoeffs []int16, xIdx, yIdx int, output []byte, outW, outH int) {
tileX := xIdx * rfxTileSize
tileY := yIdx * rfxTileSize
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
for col := 0; col < tileW; col++ {
idx := row*rfxTileSize + col
yVal := int64(yCoeffs[idx])
cb := int64(cbCoeffs[idx])
cr := int64(crCoeffs[idx])

// ICT (YCbCr → RGB) with fixed-point arithmetic matching FreeRDP
yScaled := (yVal + 4096) << 16
r := int32((cr*91916 + yScaled) >> 21)
g := int32((yScaled - cb*22527 - cr*46819) >> 21)
b := int32((cb*115992 + yScaled) >> 21)

if r < 0 {
r = 0
} else if r > 255 {
r = 255
}
if g < 0 {
g = 0
} else if g > 255 {
g = 255
}
if b < 0 {
b = 0
} else if b > 255 {
b = 255
}

outIdx := ((tileY+row)*outW + (tileX + col)) * 4
if outIdx+3 < len(output) {
output[outIdx] = byte(b)
output[outIdx+1] = byte(g)
output[outIdx+2] = byte(r)
output[outIdx+3] = 0xFF
}
}
}
}
