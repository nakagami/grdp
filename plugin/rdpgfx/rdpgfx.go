package rdpgfx

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"

	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/plugin"
)

const (
	ChannelName = plugin.RDPGFX_DVC_CHANNEL_NAME
)

// RDPGFX Command IDs (MS-RDPEGFX 2.2.2)
const (
	cmdidWireToSurface1           uint16 = 0x0001
	cmdidWireToSurface2           uint16 = 0x0002
	cmdidDeleteEncodingContext    uint16 = 0x0003
	cmdidSolidFill               uint16 = 0x0004
	cmdidSurfaceToSurface         uint16 = 0x0005
	cmdidSurfaceToCache           uint16 = 0x0006
	cmdidCacheToSurface          uint16 = 0x0007
	cmdidEvictCacheEntry         uint16 = 0x0008
	cmdidCreateSurface           uint16 = 0x0009
	cmdidDeleteSurface           uint16 = 0x000A
	cmdidStartFrame              uint16 = 0x000B
	cmdidEndFrame                uint16 = 0x000C
	cmdidFrameAcknowledge        uint16 = 0x000D
	cmdidResetGraphics           uint16 = 0x000E
	cmdidMapSurfaceToOutput      uint16 = 0x000F
	cmdidCacheImportOffer        uint16 = 0x0010
	cmdidCacheImportReply        uint16 = 0x0011
	cmdidCapsAdvertise           uint16 = 0x0012
	cmdidCapsConfirm             uint16 = 0x0013
	cmdidMapSurfaceToScaledOutput uint16 = 0x0015
	cmdidMapSurfaceToScaledWindow uint16 = 0x0016
	cmdidMapSurfaceToWindow      uint16 = 0x0018
)

// Pixel Formats
const (
	pixelFormatXRGB8888 uint8 = 0x20
	pixelFormatARGB8888 uint8 = 0x21
)

// Codec IDs
const (
	codecUncompressed uint16 = 0x0000
	codecClearCodec   uint16 = 0x0003
	codecPlanar       uint16 = 0x0004
	codecProgressive  uint16 = 0x0009
)

// Capability versions and flags
const (
	capVersion8        uint32 = 0x00080004
	capVersion81       uint32 = 0x00080105
	capVersion10       uint32 = 0x000A0002
	capFlagThinClient  uint32 = 0x00000001
	capFlagSmallCache  uint32 = 0x00000002
	capFlagAVCDisabled uint32 = 0x00000020
)

const headerSize = 8

// BitmapUpdate represents a rendered bitmap region.
type BitmapUpdate struct {
	DestLeft, DestTop, DestRight, DestBottom int
	Width, Height                            int
	Bpp                                      int    // bytes per pixel (always 4)
	Data                                     []byte // BGRA pixel data
}

type surface struct {
	width, height uint16
	format        uint8
	data          []byte // BGRA, 4 bytes per pixel
	outputX       uint32
	outputY       uint32
	mapped        bool
}

type vBarEntry struct {
	pixels []byte // BGR data, 3 bytes per pixel
	count  int
}

type clearCodecCtx struct {
	vBarStorage      []vBarEntry
	shortVBarStorage []vBarEntry
	vBarCursor       int
	shortVBarCursor  int
}

func newClearCodecCtx() *clearCodecCtx {
	return &clearCodecCtx{
		vBarStorage:      make([]vBarEntry, 32768),
		shortVBarStorage: make([]vBarEntry, 16384),
	}
}

type cacheEntry struct {
	data          []byte // BGRA pixel data
	width, height int
}

// GfxHandler implements the RDPGFX (MS-RDPEGFX) protocol.
type GfxHandler struct {
	surfaces      map[uint16]*surface
	cacheEntries  map[uint16]cacheEntry
	clearCtx      *clearCodecCtx
	zgfx          *zgfxContext
	progressive   *rfxProgressiveDecoder
	framesDecoded uint32
	sendFn        func(data []byte)
	onBitmap      func([]BitmapUpdate)
}

// NewGfxHandler creates a new RDPGFX handler.
func NewGfxHandler(onBitmap func([]BitmapUpdate)) *GfxHandler {
	return &GfxHandler{
		surfaces:     make(map[uint16]*surface),
		cacheEntries: make(map[uint16]cacheEntry),
		clearCtx:     newClearCodecCtx(),
		zgfx:         newZgfxContext(),
		progressive:  newRfxProgressiveDecoder(),
		onBitmap:     onBitmap,
	}
}

// SetSendFunc sets the function used to send RDPGFX responses via DVC.
func (g *GfxHandler) SetSendFunc(fn func([]byte)) {
	g.sendFn = fn
}

// OnChannelCreated is called after the DVC CREATE_RSP has been sent.
// It sends CAPS_ADVERTISE to the server to initiate the RDPGFX pipeline.
func (g *GfxHandler) OnChannelCreated() {
	g.sendCapsAdvertise()
}

// sendCapsAdvertise sends RDPGFX_CAPS_ADVERTISE_PDU to the server.
// The client must advertise its capabilities before the server will
// send any graphics data (MS-RDPEGFX 2.2.3.1).
func (g *GfxHandler) sendCapsAdvertise() {
	p := &bytes.Buffer{}
	// capsSetCount
	core.WriteUInt16LE(1, p)
	// RDPGFX_CAPSET: version 8.0
	core.WriteUInt32LE(capVersion8, p)
	core.WriteUInt32LE(4, p) // capsDataLength
	core.WriteUInt32LE(capFlagThinClient|capFlagSmallCache|capFlagAVCDisabled, p)
	g.sendPdu(cmdidCapsAdvertise, p.Bytes())
	slog.Info("RDPGFX: sent CAPS_ADVERTISE (v8.0, THINCLIENT|SMALLCACHE|AVC_DISABLED)")
}

// ZGFX segment descriptors (MS-RDPEGFX 2.2.4)
const (
	zgfxSingle    = 0xE0
	zgfxMultipart = 0xE1

	zgfxCompressedRDP8 = 0x04
)

// Process handles a complete RDPGFX payload (may contain multiple PDUs).
// Data arrives wrapped in ZGFX (RDP8 Bulk Compression) segments (MS-RDPEGFX 2.2.4).
func (g *GfxHandler) Process(data []byte) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("RDPGFX: panic in Process", "err", r)
		}
	}()
	if len(data) < 1 {
		return
	}

	descriptor := data[0]
	switch descriptor {
	case zgfxSingle:
		if len(data) < 2 {
			return
		}
		raw := g.decompressSegment(data[1:])
		if raw != nil {
			g.processPDUs(raw)
		}
	case zgfxMultipart:
		g.processMultipart(data[1:])
	default:
		slog.Warn(fmt.Sprintf("RDPGFX: unknown ZGFX descriptor 0x%02X, trying raw", descriptor))
		g.processPDUs(data)
	}
}

// decompressSegment handles a single ZGFX segment (after the descriptor byte).
// First byte is RDP8_BULK_ENCODED_DATA header:
//   bits 0-3: compression type (0x04 = RDP8)
//   bit 5: PACKET_COMPRESSED (0x20)
func (g *GfxHandler) decompressSegment(seg []byte) []byte {
	if len(seg) < 1 {
		return nil
	}
	header := seg[0]
	payload := seg[1:]
	if header&0x20 != 0 {
		// PACKET_COMPRESSED: use ZGFX decompression
		return g.zgfx.Decompress(payload)
	}
	// Not compressed: raw data
	return payload
}

// processMultipart handles ZGFX multipart segments (MS-RDPEGFX 2.2.4.1).
func (g *GfxHandler) processMultipart(data []byte) {
	if len(data) < 6 {
		return
	}
	r := bytes.NewReader(data)
	segCount, _ := core.ReadUint16LE(r)
	_, _ = core.ReadUInt32LE(r) // uncompressedSize

	buf := &bytes.Buffer{}
	for i := uint16(0); i < segCount; i++ {
		segSize, err := core.ReadUInt32LE(r)
		if err != nil {
			return
		}
		segData, err := core.ReadBytes(int(segSize), r)
		if err != nil {
			return
		}
		raw := g.decompressSegment(segData)
		if raw != nil {
			buf.Write(raw)
		}
	}
	g.processPDUs(buf.Bytes())
}

// processPDUs parses one or more RDPGFX PDUs from raw (uncompressed) data.
func (g *GfxHandler) processPDUs(data []byte) {
	for offset := 0; offset+headerSize <= len(data); {
		cmdId := binary.LittleEndian.Uint16(data[offset:])
		pduLength := binary.LittleEndian.Uint32(data[offset+4:])
		if pduLength < uint32(headerSize) || int(pduLength) > len(data)-offset {
			slog.Warn("RDPGFX: invalid PDU", "cmdId", cmdId, "pduLen", pduLength,
				"offset", offset, "dataLen", len(data))
			break
		}
		pduData := data[offset+headerSize : offset+int(pduLength)]
		g.dispatch(cmdId, pduData)
		offset += int(pduLength)
	}
}

func (g *GfxHandler) dispatch(cmdId uint16, data []byte) {
	slog.Info(fmt.Sprintf("RDPGFX: cmd=0x%04X len=%d", cmdId, len(data)))
	switch cmdId {
	case cmdidCapsConfirm:
		g.onCapsConfirm(data)
	case cmdidResetGraphics:
		g.onResetGraphics(data)
	case cmdidCreateSurface:
		g.onCreateSurface(data)
	case cmdidDeleteSurface:
		g.onDeleteSurface(data)
	case cmdidMapSurfaceToOutput:
		g.onMapSurfaceToOutput(data)
	case cmdidStartFrame:
		// nothing to do
	case cmdidEndFrame:
		g.onEndFrame(data)
	case cmdidWireToSurface1:
		g.onWireToSurface1(data)
	case cmdidWireToSurface2:
		g.onWireToSurface2(data)
	case cmdidSolidFill:
		g.onSolidFill(data)
	case cmdidCacheToSurface:
		g.onCacheToSurface(data)
	case cmdidEvictCacheEntry:
		g.onEvictCacheEntry(data)
	case cmdidCacheImportOffer:
		g.onCacheImportOffer()
	case cmdidMapSurfaceToWindow, cmdidMapSurfaceToScaledOutput, cmdidMapSurfaceToScaledWindow:
		// ignored
	default:
		slog.Debug(fmt.Sprintf("RDPGFX: unhandled cmd 0x%04X", cmdId))
	}
}

func (g *GfxHandler) sendPdu(cmdId uint16, payload []byte) {
	if g.sendFn == nil {
		return
	}
	b := &bytes.Buffer{}
	core.WriteUInt16LE(cmdId, b)
	core.WriteUInt16LE(0, b) // flags
	core.WriteUInt32LE(uint32(headerSize+len(payload)), b)
	b.Write(payload)
	g.sendFn(b.Bytes())
}

// --- Command Handlers ---

func (g *GfxHandler) onCapsConfirm(data []byte) {
	if len(data) < 12 {
		slog.Info("RDPGFX: CAPS_CONFIRM received (short)")
		return
	}
	r := bytes.NewReader(data)
	version, _ := core.ReadUInt32LE(r)
	dataLen, _ := core.ReadUInt32LE(r)
	flags := uint32(0)
	if dataLen >= 4 {
		flags, _ = core.ReadUInt32LE(r)
	}
	slog.Info(fmt.Sprintf("RDPGFX: CAPS_CONFIRM version=0x%08X flags=0x%08X", version, flags))
}

func (g *GfxHandler) onResetGraphics(data []byte) {
	if len(data) < 12 {
		return
	}
	r := bytes.NewReader(data)
	w, _ := core.ReadUInt32LE(r)
	h, _ := core.ReadUInt32LE(r)
	slog.Info(fmt.Sprintf("RDPGFX: RESET_GRAPHICS %dx%d", w, h))
	g.surfaces = make(map[uint16]*surface)
	g.clearCtx = newClearCodecCtx()
}

func (g *GfxHandler) onCreateSurface(data []byte) {
	if len(data) < 7 {
		return
	}
	r := bytes.NewReader(data)
	id, _ := core.ReadUint16LE(r)
	w, _ := core.ReadUint16LE(r)
	h, _ := core.ReadUint16LE(r)
	f, _ := core.ReadUInt8(r)
	slog.Info(fmt.Sprintf("RDPGFX: CREATE_SURFACE id=%d %dx%d", id, w, h))
	g.surfaces[id] = &surface{
		width: w, height: h, format: f,
		data: make([]byte, int(w)*int(h)*4),
	}
}

func (g *GfxHandler) onDeleteSurface(data []byte) {
	if len(data) < 2 {
		return
	}
	id := binary.LittleEndian.Uint16(data)
	delete(g.surfaces, id)
}

func (g *GfxHandler) onMapSurfaceToOutput(data []byte) {
	if len(data) < 12 {
		return
	}
	r := bytes.NewReader(data)
	id, _ := core.ReadUint16LE(r)
	core.ReadUint16LE(r) // reserved
	ox, _ := core.ReadUInt32LE(r)
	oy, _ := core.ReadUInt32LE(r)
	slog.Info(fmt.Sprintf("RDPGFX: MAP_SURFACE id=%d → (%d,%d)", id, ox, oy))
	if s, ok := g.surfaces[id]; ok {
		s.outputX = ox
		s.outputY = oy
		s.mapped = true
	}
}

func (g *GfxHandler) onEndFrame(data []byte) {
	if len(data) < 4 {
		return
	}
	frameId := binary.LittleEndian.Uint32(data)
	g.framesDecoded++
	p := &bytes.Buffer{}
	core.WriteUInt32LE(0xFFFFFFFF, p) // queueDepth
	core.WriteUInt32LE(frameId, p)
	core.WriteUInt32LE(g.framesDecoded, p)
	g.sendPdu(cmdidFrameAcknowledge, p.Bytes())
}

func (g *GfxHandler) onWireToSurface1(data []byte) {
	if len(data) < 17 {
		return
	}
	r := bytes.NewReader(data)
	surfId, _ := core.ReadUint16LE(r)
	codecId, _ := core.ReadUint16LE(r)
	pixFmt, _ := core.ReadUInt8(r)
	left, _ := core.ReadUint16LE(r)
	top, _ := core.ReadUint16LE(r)
	right, _ := core.ReadUint16LE(r)
	bottom, _ := core.ReadUint16LE(r)
	bmpLen, _ := core.ReadUInt32LE(r)
	bmpData, _ := core.ReadBytes(int(bmpLen), r)

	w := int(right - left)
	h := int(bottom - top)
	if w <= 0 || h <= 0 {
		return
	}

	s, ok := g.surfaces[surfId]
	if !ok {
		return
	}

	var decoded []byte
	switch codecId {
	case codecUncompressed:
		decoded = decodeUncompressed(bmpData, w, h, pixFmt)
	case codecPlanar:
		decoded = decodePlanar(bmpData, w, h)
	case codecClearCodec:
		decoded = g.clearCtx.decode(bmpData, w, h)
	default:
		slog.Debug(fmt.Sprintf("RDPGFX: unsupported codec 0x%04X", codecId))
		return
	}
	if decoded == nil {
		return
	}

	blitToSurface(s, int(left), int(top), w, h, decoded)
	g.emitBitmap(s, int(left), int(top), w, h, decoded)
}

// onWireToSurface2 handles RDPGFX_WIRE_TO_SURFACE_PDU_2 (MS-RDPEGFX 2.2.2.2).
// Used in RDPGFX v8.1+ with progressive or AVC codecs.
func (g *GfxHandler) onWireToSurface2(data []byte) {
	if len(data) < 13 {
		return
	}
	r := bytes.NewReader(data)
	surfId, _ := core.ReadUint16LE(r)
	codecId, _ := core.ReadUint16LE(r)
	codecCtxId, _ := core.ReadUInt32LE(r)
	pixFmt, _ := core.ReadUInt8(r)
	bmpLen, _ := core.ReadUInt32LE(r)
	bmpData, _ := core.ReadBytes(int(bmpLen), r)

	s, ok := g.surfaces[surfId]
	if !ok {
		return
	}

	w := int(s.width)
	h := int(s.height)

	var decoded []byte
	switch codecId {
	case codecUncompressed:
		decoded = decodeUncompressed(bmpData, w, h, pixFmt)
		blitToSurface(s, 0, 0, w, h, decoded)
		g.emitBitmap(s, 0, 0, w, h, decoded)
	case codecPlanar:
		decoded = decodePlanar(bmpData, w, h)
		blitToSurface(s, 0, 0, w, h, decoded)
		g.emitBitmap(s, 0, 0, w, h, decoded)
	case codecClearCodec:
		decoded = g.clearCtx.decode(bmpData, w, h)
		blitToSurface(s, 0, 0, w, h, decoded)
		g.emitBitmap(s, 0, 0, w, h, decoded)
	case codecProgressive:
		// Decode tiles directly onto the persistent surface buffer.
		// Each WTS2 PDU may contain only a subset of tiles; decoding
		// onto s.data preserves previously rendered tiles.
		rects := g.progressive.Decode(bmpData, s.data, w, h)
		for _, rc := range rects {
			// Extract the rect region from the surface for emission
			region := make([]byte, rc.w*rc.h*4)
			stride := w * 4
			for row := 0; row < rc.h; row++ {
				srcOff := (rc.y+row)*stride + rc.x*4
				dstOff := row * rc.w * 4
				if srcOff+rc.w*4 <= len(s.data) {
					copy(region[dstOff:dstOff+rc.w*4], s.data[srcOff:srcOff+rc.w*4])
				}
			}
			g.emitBitmap(s, rc.x, rc.y, rc.w, rc.h, region)
		}
	default:
		slog.Debug(fmt.Sprintf("RDPGFX: WTS2 unsupported codec 0x%04X ctxId=%d", codecId, codecCtxId))
		return
	}
}

func (g *GfxHandler) onSolidFill(data []byte) {
	if len(data) < 8 {
		return
	}
	r := bytes.NewReader(data)
	surfId, _ := core.ReadUint16LE(r)
	cb, _ := core.ReadUInt8(r)
	cg, _ := core.ReadUInt8(r)
	cr, _ := core.ReadUInt8(r)
	core.ReadUInt8(r) // XA
	fillCount, _ := core.ReadUint16LE(r)

	s, ok := g.surfaces[surfId]
	if !ok {
		return
	}

	for i := uint16(0); i < fillCount; i++ {
		left, _ := core.ReadUint16LE(r)
		top, _ := core.ReadUint16LE(r)
		right, _ := core.ReadUint16LE(r)
		bottom, _ := core.ReadUint16LE(r)
		w := int(right - left)
		h := int(bottom - top)
		if w <= 0 || h <= 0 {
			continue
		}

		stride := int(s.width) * 4
		for y := int(top); y < int(bottom) && y < int(s.height); y++ {
			for x := int(left); x < int(right) && x < int(s.width); x++ {
				idx := y*stride + x*4
				if idx+3 < len(s.data) {
					s.data[idx] = cb
					s.data[idx+1] = cg
					s.data[idx+2] = cr
					s.data[idx+3] = 0xFF
				}
			}
		}

		if s.mapped && g.onBitmap != nil {
			fillData := make([]byte, w*h*4)
			for j := 0; j < w*h; j++ {
				fillData[j*4] = cb
				fillData[j*4+1] = cg
				fillData[j*4+2] = cr
				fillData[j*4+3] = 0xFF
			}
			destL := int(s.outputX) + int(left)
			destT := int(s.outputY) + int(top)
			g.onBitmap([]BitmapUpdate{{
				DestLeft: destL, DestTop: destT,
				DestRight: destL + w - 1, DestBottom: destT + h - 1,
				Width: w, Height: h, Bpp: 4, Data: fillData,
			}})
		}
	}
}

func (g *GfxHandler) onCacheToSurface(data []byte) {
	if len(data) < 6 {
		return
	}
	r := bytes.NewReader(data)
	cacheSlot, _ := core.ReadUint16LE(r)
	surfId, _ := core.ReadUint16LE(r)
	destCount, _ := core.ReadUint16LE(r)

	ce, hasCE := g.cacheEntries[cacheSlot]
	s, hasSurf := g.surfaces[surfId]

	for i := uint16(0); i < destCount; i++ {
		dx, _ := core.ReadUint16LE(r)
		dy, _ := core.ReadUint16LE(r)
		if hasCE && hasSurf {
			blitToSurface(s, int(dx), int(dy), ce.width, ce.height, ce.data)
			g.emitBitmap(s, int(dx), int(dy), ce.width, ce.height, ce.data)
		}
	}
}

func (g *GfxHandler) onEvictCacheEntry(data []byte) {
	if len(data) < 2 {
		return
	}
	slot := binary.LittleEndian.Uint16(data)
	delete(g.cacheEntries, slot)
}

func (g *GfxHandler) onCacheImportOffer() {
	p := &bytes.Buffer{}
	core.WriteUInt16LE(0, p) // importedEntriesCount = 0
	g.sendPdu(cmdidCacheImportReply, p.Bytes())
}

// --- Helpers ---

func blitToSurface(s *surface, x, y, w, h int, src []byte) {
	stride := int(s.width) * 4
	for row := 0; row < h; row++ {
		dy := y + row
		if dy < 0 || dy >= int(s.height) {
			continue
		}
		srcOff := row * w * 4
		dstOff := dy*stride + x*4
		n := w * 4
		if dstOff >= 0 && dstOff+n <= len(s.data) && srcOff+n <= len(src) {
			copy(s.data[dstOff:dstOff+n], src[srcOff:srcOff+n])
		}
	}
}

func (g *GfxHandler) emitBitmap(s *surface, x, y, w, h int, decoded []byte) {
	if !s.mapped || g.onBitmap == nil {
		return
	}
	destL := int(s.outputX) + x
	destT := int(s.outputY) + y
	slog.Info("RDPGFX: emit bitmap", "x", destL, "y", destT, "w", w, "h", h)
	g.onBitmap([]BitmapUpdate{{
		DestLeft: destL, DestTop: destT,
		DestRight: destL + w - 1, DestBottom: destT + h - 1,
		Width: w, Height: h, Bpp: 4, Data: decoded,
	}})
}

// --- Codec: Uncompressed ---

func decodeUncompressed(data []byte, w, h int, pixFmt uint8) []byte {
	out := make([]byte, w*h*4)
	n := w * h * 4
	if len(data) >= n {
		copy(out, data[:n])
	} else {
		copy(out, data)
	}
	return out
}

// --- Codec: Planar (RDP 6.0 Bitmap Codec, MS-RDPEGDI 2.2.2.5) ---

func decodePlanar(data []byte, w, h int) []byte {
	if len(data) < 1 {
		return make([]byte, w*h*4)
	}
	header := data[0]
	rle := (header >> 5) & 1
	noAlpha := (header >> 6) & 1
	planeSize := w * h
	offset := 1

	var alphaPlane, redPlane, greenPlane, bluePlane []byte
	if rle == 0 {
		if noAlpha == 0 {
			alphaPlane, offset = readRawPlane(data, offset, planeSize)
		}
		redPlane, offset = readRawPlane(data, offset, planeSize)
		greenPlane, offset = readRawPlane(data, offset, planeSize)
		bluePlane, offset = readRawPlane(data, offset, planeSize)
	} else {
		if noAlpha == 0 {
			alphaPlane, offset = decodeNRLE(data, offset, planeSize)
		}
		redPlane, offset = decodeNRLE(data, offset, planeSize)
		greenPlane, offset = decodeNRLE(data, offset, planeSize)
		bluePlane, offset = decodeNRLE(data, offset, planeSize)
	}
	_ = offset

	applyDelta(alphaPlane, w, h)
	applyDelta(redPlane, w, h)
	applyDelta(greenPlane, w, h)
	applyDelta(bluePlane, w, h)

	out := make([]byte, planeSize*4)
	for i := 0; i < planeSize; i++ {
		a := byte(0xFF)
		if alphaPlane != nil && i < len(alphaPlane) {
			a = alphaPlane[i]
		}
		var rv, gv, bv byte
		if redPlane != nil && i < len(redPlane) {
			rv = redPlane[i]
		}
		if greenPlane != nil && i < len(greenPlane) {
			gv = greenPlane[i]
		}
		if bluePlane != nil && i < len(bluePlane) {
			bv = bluePlane[i]
		}
		out[i*4] = bv
		out[i*4+1] = gv
		out[i*4+2] = rv
		out[i*4+3] = a
	}
	return out
}

func readRawPlane(data []byte, offset, size int) ([]byte, int) {
	plane := make([]byte, size)
	end := offset + size
	if end > len(data) {
		end = len(data)
	}
	if offset < end {
		copy(plane, data[offset:end])
	}
	return plane, offset + size
}

func decodeNRLE(data []byte, offset, planeSize int) ([]byte, int) {
	out := make([]byte, planeSize)
	pos := 0
	for pos < planeSize && offset < len(data) {
		ctrl := data[offset]
		offset++
		runLen := int((ctrl >> 4) & 0x0F)
		rawLen := int(ctrl & 0x0F)

		if runLen == 15 {
			if offset >= len(data) {
				break
			}
			ext := int(data[offset])
			offset++
			runLen += ext
			if ext == 0xFF {
				if offset+1 >= len(data) {
					break
				}
				runLen += int(binary.LittleEndian.Uint16(data[offset:]))
				offset += 2
			}
		}
		for i := 0; i < runLen && pos < planeSize; i++ {
			out[pos] = 0
			pos++
		}

		if rawLen == 15 {
			if offset >= len(data) {
				break
			}
			ext := int(data[offset])
			offset++
			rawLen += ext
			if ext == 0xFF {
				if offset+1 >= len(data) {
					break
				}
				rawLen += int(binary.LittleEndian.Uint16(data[offset:]))
				offset += 2
			}
		}
		for i := 0; i < rawLen && pos < planeSize && offset < len(data); i++ {
			out[pos] = data[offset]
			pos++
			offset++
		}
	}
	return out, offset
}

func applyDelta(plane []byte, w, h int) {
	if plane == nil || len(plane) < w*h {
		return
	}
	for y := 1; y < h; y++ {
		for x := 0; x < w; x++ {
			plane[y*w+x] ^= plane[(y-1)*w+x]
		}
	}
}

// --- Codec: ClearCodec (MS-RDPEGFX 2.2.4) ---

func (ctx *clearCodecCtx) decode(data []byte, w, h int) []byte {
	if len(data) < 12 {
		return make([]byte, w*h*4)
	}
	r := bytes.NewReader(data)
	residualLen, _ := core.ReadUInt32LE(r)
	bandsLen, _ := core.ReadUInt32LE(r)
	subcodecLen, _ := core.ReadUInt32LE(r)

	out := make([]byte, w*h*4)
	if residualLen > 0 {
		residual, _ := core.ReadBytes(int(residualLen), r)
		decodeResidual(residual, w, h, out)
	}
	if bandsLen > 0 {
		bands, _ := core.ReadBytes(int(bandsLen), r)
		ctx.decodeBands(bands, w, out)
	}
	if subcodecLen > 0 {
		subcodec, _ := core.ReadBytes(int(subcodecLen), r)
		decodeSubcodec(subcodec, w, out)
	}
	return out
}

func decodeResidual(data []byte, w, h int, out []byte) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := (y*w + x) * 3
			di := (y*w + x) * 4
			if si+2 < len(data) && di+3 < len(out) {
				out[di] = data[si]     // B
				out[di+1] = data[si+1] // G
				out[di+2] = data[si+2] // R
				out[di+3] = 0xFF
			}
		}
	}
}

func (ctx *clearCodecCtx) decodeBands(data []byte, surfW int, out []byte) {
	r := bytes.NewReader(data)
	for r.Len() >= 11 {
		xStart, _ := core.ReadUint16LE(r)
		yStart, _ := core.ReadUint16LE(r)
		xEnd, _ := core.ReadUint16LE(r)
		yEnd, _ := core.ReadUint16LE(r)
		blueBg, _ := core.ReadUInt8(r)
		greenBg, _ := core.ReadUInt8(r)
		redBg, _ := core.ReadUInt8(r)

		bandHeight := int(yEnd - yStart)
		colCount := int(xEnd - xStart)
		if bandHeight <= 0 || colCount <= 0 {
			continue
		}

		for col := 0; col < colCount; col++ {
			if r.Len() < 2 {
				return
			}
			vBarHeader, _ := core.ReadUint16LE(r)
			x := int(xStart) + col

			if (vBarHeader & 0xC000) == 0xC000 {
				// SHORT_VBAR_CACHE_HIT
				idx := int(vBarHeader & 0x3FFF)
				if idx < len(ctx.shortVBarStorage) {
					entry := ctx.shortVBarStorage[idx]
					paintColumnBg(out, surfW, x, int(yStart), bandHeight, redBg, greenBg, blueBg)
					paintVBarPixels(out, surfW, x, int(yStart), 0, entry)
				}
			} else if (vBarHeader & 0xC000) == 0x4000 {
				// SHORT_VBAR_CACHE_MISS
				pixCount := int(vBarHeader & 0x3FFF)
				if r.Len() < 1 {
					return
				}
				yOn, _ := core.ReadUInt8(r)
				pixels := make([]byte, pixCount*3)
				if r.Len() >= pixCount*3 {
					rp, _ := core.ReadBytes(pixCount*3, r)
					copy(pixels, rp)
				} else {
					rp, _ := core.ReadBytes(r.Len(), r)
					copy(pixels, rp)
				}
				entry := vBarEntry{pixels: pixels, count: pixCount}
				if ctx.shortVBarCursor < len(ctx.shortVBarStorage) {
					ctx.shortVBarStorage[ctx.shortVBarCursor] = entry
				}
				ctx.shortVBarCursor = (ctx.shortVBarCursor + 1) % len(ctx.shortVBarStorage)
				paintColumnBg(out, surfW, x, int(yStart), bandHeight, redBg, greenBg, blueBg)
				paintVBarPixels(out, surfW, x, int(yStart), int(yOn), entry)
			} else if (vBarHeader & 0x8000) == 0x8000 {
				// VBAR_CACHE_HIT
				idx := int(vBarHeader & 0x7FFF)
				if idx < len(ctx.vBarStorage) {
					entry := ctx.vBarStorage[idx]
					paintVBarPixels(out, surfW, x, int(yStart), 0, entry)
				}
			} else {
				// VBAR_CACHE_MISS
				pixCount := int(vBarHeader & 0x7FFF)
				pixels := make([]byte, pixCount*3)
				if r.Len() >= pixCount*3 {
					rp, _ := core.ReadBytes(pixCount*3, r)
					copy(pixels, rp)
				} else {
					rp, _ := core.ReadBytes(r.Len(), r)
					copy(pixels, rp)
				}
				entry := vBarEntry{pixels: pixels, count: pixCount}
				if ctx.vBarCursor < len(ctx.vBarStorage) {
					ctx.vBarStorage[ctx.vBarCursor] = entry
				}
				ctx.vBarCursor = (ctx.vBarCursor + 1) % len(ctx.vBarStorage)
				paintVBarPixels(out, surfW, x, int(yStart), 0, entry)
			}
		}
	}
}

func paintColumnBg(out []byte, surfW, x, yStart, height int, r, g, b uint8) {
	for y := 0; y < height; y++ {
		dy := yStart + y
		idx := (dy*surfW + x) * 4
		if idx+3 < len(out) {
			out[idx] = b
			out[idx+1] = g
			out[idx+2] = r
			out[idx+3] = 0xFF
		}
	}
}

func paintVBarPixels(out []byte, surfW, x, yStart, yOn int, entry vBarEntry) {
	for y := 0; y < entry.count; y++ {
		si := y * 3
		dy := yStart + yOn + y
		di := (dy*surfW + x) * 4
		if si+2 < len(entry.pixels) && di+3 < len(out) {
			out[di] = entry.pixels[si]     // B
			out[di+1] = entry.pixels[si+1] // G
			out[di+2] = entry.pixels[si+2] // R
			out[di+3] = 0xFF
		}
	}
}

func decodeSubcodec(data []byte, surfW int, out []byte) {
	r := bytes.NewReader(data)
	for r.Len() >= 13 {
		xStart, _ := core.ReadUint16LE(r)
		yStart, _ := core.ReadUint16LE(r)
		width, _ := core.ReadUint16LE(r)
		height, _ := core.ReadUint16LE(r)
		bmpLen, _ := core.ReadUInt32LE(r)
		subcodecId, _ := core.ReadUInt8(r)
		if int(bmpLen) > r.Len() {
			break
		}
		bmpData, _ := core.ReadBytes(int(bmpLen), r)

		if subcodecId == 0 {
			// RAW BGR
			for y := 0; y < int(height); y++ {
				for x := 0; x < int(width); x++ {
					si := (y*int(width) + x) * 3
					dy := int(yStart) + y
					dx := int(xStart) + x
					di := (dy*surfW + dx) * 4
					if si+2 < len(bmpData) && di+3 < len(out) {
						out[di] = bmpData[si]
						out[di+1] = bmpData[si+1]
						out[di+2] = bmpData[si+2]
						out[di+3] = 0xFF
					}
				}
			}
		}
		// Skip NSCodec (1) and glyph (2) subcodecs
	}
}
