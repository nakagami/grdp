package rdpgfx

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/plugin"
)

// regionPool reuses byte slices for progressive codec rectangle extraction,
// avoiding per-rectangle allocations that cause GC pressure.
var regionPool = sync.Pool{
	New: func() any { return []byte(nil) },
}

const (
	ChannelName = plugin.RDPGFX_DVC_CHANNEL_NAME
)

// RDPGFX Command IDs (MS-RDPEGFX 2.2.2)
const (
	cmdidWireToSurface1           uint16 = 0x0001
	cmdidWireToSurface2           uint16 = 0x0002
	cmdidDeleteEncodingContext    uint16 = 0x0003
	cmdidSolidFill                uint16 = 0x0004
	cmdidSurfaceToSurface         uint16 = 0x0005
	cmdidSurfaceToCache           uint16 = 0x0006
	cmdidCacheToSurface           uint16 = 0x0007
	cmdidEvictCacheEntry          uint16 = 0x0008
	cmdidCreateSurface            uint16 = 0x0009
	cmdidDeleteSurface            uint16 = 0x000A
	cmdidStartFrame               uint16 = 0x000B
	cmdidEndFrame                 uint16 = 0x000C
	cmdidFrameAcknowledge         uint16 = 0x000D
	cmdidResetGraphics            uint16 = 0x000E
	cmdidMapSurfaceToOutput       uint16 = 0x000F
	cmdidCacheImportOffer         uint16 = 0x0010
	cmdidCacheImportReply         uint16 = 0x0011
	cmdidCapsAdvertise            uint16 = 0x0012
	cmdidCapsConfirm              uint16 = 0x0013
	cmdidMapSurfaceToScaledOutput  uint16 = 0x0015
	cmdidMapSurfaceToScaledWindow  uint16 = 0x0016
	cmdidMapSurfaceToScaledOutputV2 uint16 = 0x0017 // v10.6+
	cmdidMapSurfaceToWindow        uint16 = 0x0018
)

// Pixel Formats
const (
	pixelFormatXRGB8888 uint8 = 0x20
	pixelFormatARGB8888 uint8 = 0x21
)

// Codec IDs (MS-RDPEGFX 2.2.2.1 / FreeRDP rdpgfx.h)
const (
	codecUncompressed uint16 = 0x0000
	codecCaVideo      uint16 = 0x0003 // RDPGFX_CODECID_CAVIDEO (RemoteFX tiles)
	codecPlanar       uint16 = 0x0004
	codecProgressive  uint16 = 0x0009
	codecAVC420       uint16 = 0x000B
	codecAVC444       uint16 = 0x000E
	codecAVC444v2     uint16 = 0x000F
)

// Capability versions and flags
const (
	capVersion8          uint32 = 0x00080004
	capVersion81         uint32 = 0x00080105
	capVersion10         uint32 = 0x000A0002
	capVersion101        uint32 = 0x000A0100
	capVersion102        uint32 = 0x000A0200
	capVersion103        uint32 = 0x000A0301
	capVersion104        uint32 = 0x000A0400
	capVersion105        uint32 = 0x000A0502
	capVersion106        uint32 = 0x000A0600
	capVersion1061       uint32 = 0x000A0601
	capVersion107        uint32 = 0x000A0701
	capFlagThinClient    uint32 = 0x00000001
	capFlagSmallCache    uint32 = 0x00000002
	capFlagAVC420Enabled uint32 = 0x00000010 // v8.1: explicitly enable AVC420
	capFlagAVCDisabled   uint32 = 0x00000020 // v10+: disable AVC
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
	surfaces     map[uint16]*surface
	cacheEntries map[uint16]cacheEntry
	clearCtx     *clearCodecCtx
	zgfx         *zgfxContext
	rfx          *rfxDecoder
	progressive  *rfxProgressiveDecoder
	h264dec      h264Decoder
	// framesDecoded is accessed from both read and decode goroutines.
	framesDecoded atomic.Uint32
	sendFn        func(data []byte)
	onBitmap      func([]BitmapUpdate)
	// decodeCh receives decompressed PDU data for asynchronous decode.
	decodeCh chan []byte
	// ackCh is a buffered channel of serialized ACK PDUs.  Every
	// EndFrame ACK is enqueued here and the writeLoop goroutine sends
	// each one to the server.  The server tracks outstanding frames
	// individually, so skipping ACKs causes it to stop sending.
	ackCh chan []byte
	// onKeyframeNeeded is called once when the H.264 decoder switches to
	// software and needs a fresh IDR from the server.  The caller should
	// send a refresh request (e.g. RefreshRectPDU) to trigger a new GOP.
	onKeyframeNeeded    func()
	keyframeRequested   bool
	lastKeyframeRequest time.Time
	// onDecoderBroken is called once when the H.264 decoder becomes permanently
	// unrecoverable.  The caller should reconnect the RDP session to create a
	// fresh decoder.
	onDecoderBroken        func()
	decoderBrokenNotified  bool
	// lastHardResetCount tracks the decoder's hard-reset count so we can
	// detect a new reset and immediately clear the keyframe rate-limit.
	// After a hard reset the decoder wants a fresh IDR urgently; we must
	// not wait up to 3 s for the rate-limit window to expire.
	lastHardResetCount int
}

// NewGfxHandler creates a new RDPGFX handler.
func NewGfxHandler(onBitmap func([]BitmapUpdate)) *GfxHandler {
	g := &GfxHandler{
		surfaces:     make(map[uint16]*surface),
		cacheEntries: make(map[uint16]cacheEntry),
		clearCtx:     newClearCodecCtx(),
		zgfx:         newZgfxContext(),
		rfx:          newRfxDecoder(),
		progressive:  newRfxProgressiveDecoder(),
		h264dec:      newH264Decoder(),
		onBitmap:     onBitmap,
		decodeCh:     make(chan []byte, 256),
		ackCh:        make(chan []byte, 256),
	}
	go g.decodeLoop()
	go g.writeLoop()
	return g
}

// SetSendFunc sets the function used to send RDPGFX responses via DVC.
func (g *GfxHandler) SetSendFunc(fn func([]byte)) {
	g.sendFn = fn
}

// SetKeyframeNeededCallback registers a function that is called once when the
// H.264 decoder resets to software mode and needs a fresh keyframe (IDR) from
// the server.  The callback should send a screen-refresh request so the server
// starts a new GOP and decoding can resume promptly.
func (g *GfxHandler) SetKeyframeNeededCallback(fn func()) {
	g.onKeyframeNeeded = fn
}

// SetDecoderBrokenCallback registers a function that is called once when the
// H.264 decoder becomes permanently unrecoverable (all hard-reset retries
// exhausted).  The callback should reconnect the RDP session so a fresh
// decoder can be created from scratch.
func (g *GfxHandler) SetDecoderBrokenCallback(fn func()) {
	g.onDecoderBroken = fn
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

	if g.h264dec != nil {
		// Advertise capsets in ascending order (v8.0 → v10.7), matching
		// rdpyqt / FreeRDP layout so servers pick the highest common version.
		core.WriteUInt16LE(11, p) // capsSetCount

		// v8.0 — baseline fallback (no AVC)
		core.WriteUInt32LE(capVersion8, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagThinClient, p)

		// v8.1 — AVC420 via explicit flag
		core.WriteUInt32LE(capVersion81, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache|capFlagAVC420Enabled, p)

		// v10.0
		core.WriteUInt32LE(capVersion10, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		// v10.1 — 16-byte capsData (12 zero bytes after flags)
		core.WriteUInt32LE(capVersion101, p)
		core.WriteUInt32LE(16, p)
		core.WriteUInt32LE(0, p)
		core.WriteUInt32LE(0, p)
		core.WriteUInt32LE(0, p)
		core.WriteUInt32LE(0, p)

		// v10.2
		core.WriteUInt32LE(capVersion102, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		// v10.3
		core.WriteUInt32LE(capVersion103, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(0, p)

		// v10.4
		core.WriteUInt32LE(capVersion104, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		// v10.5
		core.WriteUInt32LE(capVersion105, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		// v10.6
		core.WriteUInt32LE(capVersion106, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		// v10.6.1
		core.WriteUInt32LE(capVersion1061, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		// v10.7
		core.WriteUInt32LE(capVersion107, p)
		core.WriteUInt32LE(4, p)
		core.WriteUInt32LE(capFlagSmallCache, p)

		g.sendPdu(cmdidCapsAdvertise, p.Bytes())
		slog.Debug("RDPGFX: sent CAPS_ADVERTISE (v10.7..v8.0, AVC enabled)")
	} else {
		core.WriteUInt16LE(1, p) // capsSetCount
		core.WriteUInt32LE(capVersion8, p)
		core.WriteUInt32LE(4, p) // capsDataLength
		// Use flags that intentionally cause servers to reject the RDPGFX
		// channel, forcing fallback to surface bitmap commands (NSCodec /
		// RemoteFX). We do not yet support ClearCodec (0x0008) or Planar
		// (0x0009) which servers send over RDPGFX when it stays open.
		core.WriteUInt32LE(capFlagThinClient|capFlagSmallCache|capFlagAVCDisabled, p)
		g.sendPdu(cmdidCapsAdvertise, p.Bytes())
		slog.Debug("RDPGFX: sent CAPS_ADVERTISE (v8.0)")
	}
}

// ZGFX segment descriptors (MS-RDPEGFX 2.2.4)
const (
	zgfxSingle    = 0xE0
	zgfxMultipart = 0xE1

	zgfxCompressedRDP8 = 0x04
)

// Process handles a complete RDPGFX payload (may contain multiple PDUs).
// Data arrives wrapped in ZGFX (RDP8 Bulk Compression) segments (MS-RDPEGFX 2.2.4).
//
// Called on the network read goroutine.  Decompression happens here;
// the decompressed payload is then queued for asynchronous processing
// (including frame ACKs and decode) on the decode goroutine.
// This keeps the read goroutine free from any socket.Write calls that
// could cause TCP deadlock when both sides try to write simultaneously.
func (g *GfxHandler) Process(data []byte) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("RDPGFX: panic in Process", "err", r)
		}
	}()
	if len(data) < 1 {
		return
	}

	var decompressed []byte

	descriptor := data[0]
	switch descriptor {
	case zgfxSingle:
		if len(data) < 2 {
			return
		}
		decompressed = g.decompressSegment(data[1:])
	case zgfxMultipart:
		decompressed = g.decompressMultipart(data[1:])
	default:
		slog.Warn(fmt.Sprintf("RDPGFX: unknown ZGFX descriptor 0x%02X, trying raw", descriptor))
		decompressed = data
	}

	if len(decompressed) == 0 {
		return
	}

	// decompressSegment / decompressMultipart already return owned
	// buffers (freshly allocated or copied from input) so we can hand
	// the slice directly to the async decode goroutine.
	select {
	case g.decodeCh <- decompressed:
	default:
		// Channel full — video decode is dropped, but we must still
		// ACK any EndFrame PDUs so the server's outstanding-frame
		// count stays accurate and it keeps sending.
		slog.Warn("RDPGFX: decodeCh full, dropping frame (ACKs preserved)", "queueCap", cap(g.decodeCh))
		g.ackDroppedFrames(decompressed)
	}
}

// ackDroppedFrames scans decompressed PDU data for EndFrame commands
// and sends ACKs for them.  Called on the read goroutine when decodeCh
// is full and the message is being dropped.  Without this, dropped
// EndFrames would leave the server's outstanding-frame count stuck,
// eventually causing it to stop sending entirely.
func (g *GfxHandler) ackDroppedFrames(data []byte) {
	for offset := 0; offset+headerSize <= len(data); {
		cmdId := binary.LittleEndian.Uint16(data[offset:])
		pduLength := binary.LittleEndian.Uint32(data[offset+4:])
		if pduLength < uint32(headerSize) || int(pduLength) > len(data)-offset {
			break
		}
		if cmdId == cmdidEndFrame {
			pduData := data[offset+headerSize : offset+int(pduLength)]
			if len(pduData) >= 4 {
				g.sendFrameAck(binary.LittleEndian.Uint32(pduData))
			}
		}
		offset += int(pduLength)
	}
}

// decompressSegment handles a single ZGFX segment (after the descriptor byte).
// First byte is RDP8_BULK_ENCODED_DATA header:
//
//	bits 0-3: compression type (0x04 = RDP8)
//	bit 5: PACKET_COMPRESSED (0x20)
func (g *GfxHandler) decompressSegment(seg []byte) []byte {
	if len(seg) < 1 {
		return nil
	}
	header := seg[0]
	payload := seg[1:]
	if header&0x20 != 0 {
		return g.zgfx.Decompress(payload)
	}
	g.zgfx.historyWrite(payload)
	// Return a copy: payload aliases the caller's network buffer, which
	// will be reused on the next read. Callers (Process) hand the slice
	// off to the async decode goroutine and must own the memory.
	owned := make([]byte, len(payload))
	copy(owned, payload)
	return owned
}

// decompressMultipart handles ZGFX multipart segments and returns the
// concatenated decompressed data (without processing PDUs).
func (g *GfxHandler) decompressMultipart(data []byte) []byte {
	if len(data) < 6 {
		return nil
	}
	r := bytes.NewReader(data)
	segCount, _ := core.ReadUint16LE(r)
	core.ReadUInt32LE(r) // uncompSize

	buf := &bytes.Buffer{}
	for i := uint16(0); i < segCount; i++ {
		segSize, err := core.ReadUInt32LE(r)
		if err != nil {
			return nil
		}
		segData, err := core.ReadBytes(int(segSize), r)
		if err != nil {
			return nil
		}
		raw := g.decompressSegment(segData)
		if raw != nil {
			buf.Write(raw)
		}
	}
	return buf.Bytes()
}

// decodeLoop runs in a dedicated goroutine, reading decompressed PDU data
// from decodeCh and dispatching all processing — including frame ACKs and
// heavy decode work.  Keeping socket.Write calls off the read goroutine
// avoids TCP deadlock (where both sides try to write while neither reads).
// It automatically restarts on panic.
func (g *GfxHandler) decodeLoop() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("RDPGFX: panic in decodeLoop, restarting", "err", r)
			go g.decodeLoop()
		}
	}()
	slog.Debug("RDPGFX: decodeLoop started")
	for data := range g.decodeCh {
		g.decodePDUs(data)
	}
}

// skipHeavyThreshold controls when CaVideo/progressive decode is skipped.
// When the queue has more items than this, heavy decode is skipped to drain
// the backlog quickly.  A small threshold means we decode almost every frame
// during normal playback, only skipping under severe backpressure.
const skipHeavyThreshold = 16

// decodePDUs processes all PDUs in decompressed data.
// Frame ACKs (EndFrame) are ALWAYS processed so the server gets timely
// acknowledgements.  Heavy CaVideo/progressive decode is skipped when
// the queue is significantly backed up.
func (g *GfxHandler) decodePDUs(data []byte) {
	skipHeavy := len(g.decodeCh) > skipHeavyThreshold

	for offset := 0; offset+headerSize <= len(data); {
		cmdId := binary.LittleEndian.Uint16(data[offset:])
		pduLength := binary.LittleEndian.Uint32(data[offset+4:])
		if pduLength < uint32(headerSize) || int(pduLength) > len(data)-offset {
			break
		}
		pduData := data[offset+headerSize : offset+int(pduLength)]
		g.dispatchDecode(cmdId, pduData, skipHeavy)
		offset += int(pduLength)
	}
}

// dispatchDecode routes a single PDU.  When skipHeavy is true, CaVideo
// and progressive decode are skipped to drain the queue quickly.
// EndFrame (frame ACK) is always processed regardless of skipHeavy.
func (g *GfxHandler) dispatchDecode(cmdId uint16, data []byte, skipHeavy bool) {
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
		g.onEndFrame(data) // always ACK, even when skipHeavy
	case cmdidWireToSurface1:
		g.onWireToSurface1Decode(data, skipHeavy)
	case cmdidWireToSurface2:
		g.onWireToSurface2Decode(data, skipHeavy)
	case cmdidSolidFill:
		g.onSolidFill(data)
	case cmdidCacheToSurface:
		g.onCacheToSurface(data)
	case cmdidEvictCacheEntry:
		g.onEvictCacheEntry(data)
	case cmdidCacheImportOffer:
		g.onCacheImportOffer()
	case cmdidMapSurfaceToWindow, cmdidMapSurfaceToScaledWindow:
		// ignored — we don't support per-window mapping
	case cmdidMapSurfaceToScaledOutput, cmdidMapSurfaceToScaledOutputV2:
		g.onMapSurfaceToScaledOutput(data)
	default:
		slog.Debug(fmt.Sprintf("RDPGFX: unhandled cmd 0x%04X", cmdId))
	}
}

// writeLoop runs in a dedicated goroutine.  It reads serialized ACK
// PDUs from ackCh and sends each one via sendFn.  Every ACK must reach
// the server — the server tracks outstanding frames individually and
// stops sending if ACKs are missing.  Automatically restarts on panic.
func (g *GfxHandler) writeLoop() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("RDPGFX: panic in writeLoop, restarting", "err", r)
			go g.writeLoop()
		}
	}()
	for pdu := range g.ackCh {
		if g.sendFn != nil {
			g.sendFn(pdu)
		}
	}
}

// sendPdu sends a PDU synchronously.  Used for rare control messages
// (CapsAdvertise, CacheImportReply) that must not be dropped.
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

// sendPduAsync enqueues a PDU for the writeLoop goroutine to send.
// Every ACK must be delivered to the server, so this uses a buffered
// channel rather than a single-value "latest" slot.
func (g *GfxHandler) sendPduAsync(cmdId uint16, payload []byte) {
	b := &bytes.Buffer{}
	core.WriteUInt16LE(cmdId, b)
	core.WriteUInt16LE(0, b) // flags
	core.WriteUInt32LE(uint32(headerSize+len(payload)), b)
	b.Write(payload)
	select {
	case g.ackCh <- b.Bytes():
	default:
		slog.Warn("RDPGFX: ackCh full, ACK dropped")
	}
}

// --- Command Handlers ---

func (g *GfxHandler) onCapsConfirm(data []byte) {
	if len(data) < 12 {
		slog.Debug("RDPGFX: CAPS_CONFIRM received (short)")
		return
	}
	r := bytes.NewReader(data)
	version, _ := core.ReadUInt32LE(r)
	dataLen, _ := core.ReadUInt32LE(r)
	flags := uint32(0)
	if dataLen >= 4 {
		flags, _ = core.ReadUInt32LE(r)
	}
	slog.Debug(fmt.Sprintf("RDPGFX: CAPS_CONFIRM version=0x%08X flags=0x%08X", version, flags))
}

func (g *GfxHandler) onResetGraphics(data []byte) {
	if len(data) < 12 {
		return
	}
	r := bytes.NewReader(data)
	w, _ := core.ReadUInt32LE(r)
	h, _ := core.ReadUInt32LE(r)
	slog.Debug(fmt.Sprintf("RDPGFX: RESET_GRAPHICS %dx%d", w, h))
	g.surfaces = make(map[uint16]*surface)
	g.clearCtx = newClearCodecCtx()
	g.framesDecoded.Store(0)
	if g.h264dec != nil {
		g.h264dec.Close()
		g.h264dec = newH264Decoder()
	}
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
	slog.Debug(fmt.Sprintf("RDPGFX: CREATE_SURFACE id=%d %dx%d", id, w, h))
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
	slog.Debug(fmt.Sprintf("RDPGFX: MAP_SURFACE id=%d → (%d,%d)", id, ox, oy))
	if s, ok := g.surfaces[id]; ok {
		s.outputX = ox
		s.outputY = oy
		s.mapped = true
	}
}

func (g *GfxHandler) onMapSurfaceToScaledOutput(data []byte) {
	if len(data) < 20 {
		return
	}
	r := bytes.NewReader(data)
	id, _ := core.ReadUint16LE(r)
	core.ReadUint16LE(r) // reserved
	ox, _ := core.ReadUInt32LE(r)
	oy, _ := core.ReadUInt32LE(r)
	core.ReadUInt32LE(r) // targetWidth
	core.ReadUInt32LE(r) // targetHeight
	slog.Debug(fmt.Sprintf("RDPGFX: MAP_SURFACE_SCALED id=%d → (%d,%d)", id, ox, oy))
	if s, ok := g.surfaces[id]; ok {
		s.outputX = ox
		s.outputY = oy
		s.mapped = true
	}
}

// sendFrameAck builds and queues a FRAME_ACKNOWLEDGE PDU.
// Safe to call from any goroutine (uses atomic framesDecoded and
// mutex-protected latestAck).
func (g *GfxHandler) sendFrameAck(frameId uint32) {
	decoded := g.framesDecoded.Add(1)
	p := &bytes.Buffer{}
	core.WriteUInt32LE(0, p)
	core.WriteUInt32LE(frameId, p)
	core.WriteUInt32LE(decoded, p)
	g.sendPduAsync(cmdidFrameAcknowledge, p.Bytes())
}

func (g *GfxHandler) onEndFrame(data []byte) {
	if len(data) < 4 {
		return
	}
	g.sendFrameAck(binary.LittleEndian.Uint32(data))
}

func (g *GfxHandler) onWireToSurface1Decode(data []byte, skipHeavy bool) {
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

	slog.Debug(fmt.Sprintf("RDPGFX: WTS1 surfId=%d codecId=0x%04X %dx%d bmpLen=%d", surfId, codecId, right-left, bottom-top, bmpLen))

	w := int(right - left)
	h := int(bottom - top)
	if w <= 0 || h <= 0 {
		return
	}

	s, ok := g.surfaces[surfId]
	if !ok {
		return
	}

	// CaVideo (0x0003) carries RFX tile-encoded data; decode onto the
	// persistent surface buffer like the progressive codec in WTS2.
	if codecId == codecCaVideo {
		if skipHeavy {
			return
		}
		rects := g.rfx.Decode(bmpData, int(left), int(top), s.data, int(s.width), int(s.height))
		if !s.mapped || g.onBitmap == nil || len(rects) == 0 {
			return
		}
		updates := make([]BitmapUpdate, 0, len(rects))
		stride := int(s.width) * 4
		for _, rc := range rects {
			needed := rc.w * rc.h * 4
			region := make([]byte, needed)
			rowBytes := rc.w * 4
			for row := 0; row < rc.h; row++ {
				srcOff := (rc.y+row)*stride + rc.x*4
				dstOff := row * rowBytes
				if srcOff+rowBytes <= len(s.data) {
					copy(region[dstOff:dstOff+rowBytes], s.data[srcOff:srcOff+rowBytes])
				}
			}
			destL := int(s.outputX) + rc.x
			destT := int(s.outputY) + rc.y
			updates = append(updates, BitmapUpdate{
				DestLeft: destL, DestTop: destT,
				DestRight: destL + rc.w - 1, DestBottom: destT + rc.h - 1,
				Width: rc.w, Height: rc.h, Bpp: 4, Data: region,
			})
		}
		g.onBitmap(updates)
		return
	}

	var decoded []byte
	var avcRegions []avcRect
	switch codecId {
	case codecUncompressed:
		decoded = decodeUncompressed(bmpData, w, h, pixFmt)
	case codecPlanar:
		decoded = decodePlanar(bmpData, w, h)
	case codecAVC420:
		decoded, avcRegions = g.decodeAVC420(bmpData, w, h)
	case codecAVC444, codecAVC444v2:
		decoded, avcRegions = g.decodeAVC444(bmpData, w, h)
	default:
		slog.Warn(fmt.Sprintf("RDPGFX: unsupported codec 0x%04X in WTS1 (surf=%d %dx%d bmpLen=%d)", codecId, surfId, w, h, bmpLen))
		return
	}
	if decoded == nil {
		return
	}

	if len(avcRegions) > 0 && shouldUseAVCRegions(avcRegions, w, h) {
		g.blitAndEmitAVCRegions(s, int(left), int(top), w, h, decoded, avcRegions)
		return
	}

	blitToSurface(s, int(left), int(top), w, h, decoded)
	g.emitBitmap(s, int(left), int(top), w, h, decoded)
}

// onWireToSurface2Decode handles RDPGFX_WIRE_TO_SURFACE_PDU_2 (MS-RDPEGFX 2.2.2.2).
func (g *GfxHandler) onWireToSurface2Decode(data []byte, skipHeavy bool) {
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

	slog.Debug(fmt.Sprintf("RDPGFX: WTS2 surfId=%d codecId=0x%04X %dx%d bmpLen=%d", surfId, codecId, w, h, bmpLen))

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
	case codecCaVideo:
		if skipHeavy {
			break // frame drop
		}
		rects := g.rfx.Decode(bmpData, 0, 0, s.data, w, h)
		if s.mapped && g.onBitmap != nil && len(rects) > 0 {
			updates := make([]BitmapUpdate, 0, len(rects))
			stride := w * 4
			for _, rc := range rects {
				needed := rc.w * rc.h * 4
				region := make([]byte, needed)
				rowBytes := rc.w * 4
				for row := 0; row < rc.h; row++ {
					srcOff := (rc.y+row)*stride + rc.x*4
					dstOff := row * rowBytes
					if srcOff+rowBytes <= len(s.data) {
						copy(region[dstOff:dstOff+rowBytes], s.data[srcOff:srcOff+rowBytes])
					}
				}
				destL := int(s.outputX) + rc.x
				destT := int(s.outputY) + rc.y
				updates = append(updates, BitmapUpdate{
					DestLeft: destL, DestTop: destT,
					DestRight: destL + rc.w - 1, DestBottom: destT + rc.h - 1,
					Width: rc.w, Height: rc.h, Bpp: 4, Data: region,
				})
			}
			g.onBitmap(updates)
		}
	case codecAVC420:
		decoded, avcRegions := g.decodeAVC420(bmpData, w, h)
		if decoded != nil {
			if len(avcRegions) > 0 && shouldUseAVCRegions(avcRegions, w, h) {
				g.blitAndEmitAVCRegions(s, 0, 0, w, h, decoded, avcRegions)
			} else {
				blitToSurface(s, 0, 0, w, h, decoded)
				g.emitBitmap(s, 0, 0, w, h, decoded)
			}
		}
	case codecAVC444, codecAVC444v2:
		decoded, avcRegions := g.decodeAVC444(bmpData, w, h)
		if decoded != nil {
			if len(avcRegions) > 0 && shouldUseAVCRegions(avcRegions, w, h) {
				g.blitAndEmitAVCRegions(s, 0, 0, w, h, decoded, avcRegions)
			} else {
				blitToSurface(s, 0, 0, w, h, decoded)
				g.emitBitmap(s, 0, 0, w, h, decoded)
			}
		}
	case codecProgressive:
		if skipHeavy {
			break // frame drop
		}
		// Decode tiles directly onto the persistent surface buffer.
		rects := g.progressive.Decode(bmpData, s.data, w, h)
		for _, rc := range rects {
			needed := rc.w * rc.h * 4
			region := regionPool.Get().([]byte)
			if cap(region) < needed {
				region = make([]byte, needed)
			} else {
				region = region[:needed]
			}
			stride := w * 4
			rowBytes := rc.w * 4
			for row := 0; row < rc.h; row++ {
				srcOff := (rc.y+row)*stride + rc.x*4
				dstOff := row * rowBytes
				if srcOff+rowBytes <= len(s.data) {
					copy(region[dstOff:dstOff+rowBytes], s.data[srcOff:srcOff+rowBytes])
				}
			}
			g.emitBitmap(s, rc.x, rc.y, rc.w, rc.h, region)
			regionPool.Put(region)
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

	stride := int(s.width) * 4
	// Pre-compose a single BGRA pixel for row-level fill
	pixel := [4]byte{cb, cg, cr, 0xFF}

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

		// Clamp to surface bounds
		yEnd := int(bottom)
		if yEnd > int(s.height) {
			yEnd = int(s.height)
		}
		xEnd := int(right)
		if xEnd > int(s.width) {
			xEnd = int(s.width)
		}

		// Fill the first row, then copy() it to subsequent rows
		rowStart := int(top)*stride + int(left)*4
		rowBytes := (xEnd - int(left)) * 4
		if rowStart+rowBytes <= len(s.data) {
			for x := 0; x < rowBytes; x += 4 {
				copy(s.data[rowStart+x:rowStart+x+4], pixel[:])
			}
			for y := int(top) + 1; y < yEnd; y++ {
				dst := y*stride + int(left)*4
				if dst+rowBytes <= len(s.data) {
					copy(s.data[dst:dst+rowBytes], s.data[rowStart:rowStart+rowBytes])
				}
			}
		}

		if s.mapped && g.onBitmap != nil {
			// Build fill data: fill first row, then replicate
			fillData := make([]byte, w*h*4)
			rowW := w * 4
			for x := 0; x < rowW; x += 4 {
				copy(fillData[x:x+4], pixel[:])
			}
			for row := 1; row < h; row++ {
				copy(fillData[row*rowW:(row+1)*rowW], fillData[:rowW])
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
	// Hoist the per-pixel nil/length checks: clamp each plane to
	// `planeSize` (zero-fill missing planes) so the inner loop has no
	// branches and the bounds checks are eliminated.
	rp := planeOrZero(redPlane, planeSize)
	gp := planeOrZero(greenPlane, planeSize)
	bp := planeOrZero(bluePlane, planeSize)
	ap := alphaPlane
	hasAlpha := ap != nil && len(ap) >= planeSize
	if hasAlpha {
		ap = ap[:planeSize]
		for i := 0; i < planeSize; i++ {
			j := i * 4
			out[j] = bp[i]
			out[j+1] = gp[i]
			out[j+2] = rp[i]
			out[j+3] = ap[i]
		}
	} else {
		for i := 0; i < planeSize; i++ {
			j := i * 4
			out[j] = bp[i]
			out[j+1] = gp[i]
			out[j+2] = rp[i]
			out[j+3] = 0xFF
		}
	}
	return out
}

// planeOrZero returns a slice of exactly `size` bytes, either the input
// plane (truncated if longer) or a zero-filled buffer when the plane is
// nil or short.  Used to drop per-pixel nil/bounds checks in decodePlanar.
func planeOrZero(plane []byte, size int) []byte {
	if len(plane) >= size {
		return plane[:size]
	}
	out := make([]byte, size)
	copy(out, plane)
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
	// Process row-by-row: previous row XORs into current row using
	// fixed-length slices so the compiler eliminates per-element
	// bounds checks and the index multiplications hoist.
	for y := 1; y < h; y++ {
		base := y * w
		prev := plane[base-w : base : base]
		cur := plane[base : base+w : base+w]
		for x := 0; x < w; x++ {
			cur[x] ^= prev[x]
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
		rowDstStart := y * w * 4
		rowSrcStart := y * w * 3
		rowDstEnd := rowDstStart + w*4
		rowSrcEnd := rowSrcStart + w*3
		if rowDstEnd > len(out) || rowSrcEnd > len(data) {
			return
		}
		dst := out[rowDstStart:rowDstEnd:rowDstEnd]
		src := data[rowSrcStart:rowSrcEnd:rowSrcEnd]
		for x := 0; x < w; x++ {
			si := x * 3
			di := x * 4
			dst[di] = src[si]
			dst[di+1] = src[si+1]
			dst[di+2] = src[si+2]
			dst[di+3] = 0xFF
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
	if x < 0 || surfW <= 0 || x >= surfW {
		return
	}
	for y := 0; y < height; y++ {
		dy := yStart + y
		idx := (dy*surfW + x) * 4
		if idx < 0 || idx+4 > len(out) {
			continue
		}
		px := out[idx : idx+4 : idx+4]
		px[0] = b
		px[1] = g
		px[2] = r
		px[3] = 0xFF
	}
}

func paintVBarPixels(out []byte, surfW, x, yStart, yOn int, entry vBarEntry) {
	if x < 0 || surfW <= 0 || x >= surfW {
		return
	}
	pixels := entry.pixels
	for y := 0; y < entry.count; y++ {
		si := y * 3
		dy := yStart + yOn + y
		di := (dy*surfW + x) * 4
		if si+3 > len(pixels) || di < 0 || di+4 > len(out) {
			continue
		}
		src := pixels[si : si+3 : si+3]
		px := out[di : di+4 : di+4]
		px[0] = src[0]
		px[1] = src[1]
		px[2] = src[2]
		px[3] = 0xFF
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
			rowSrc := int(width) * 3
			rowDst := surfW * 4
			for y := 0; y < int(height); y++ {
				srcStart := y * rowSrc
				srcEnd := srcStart + rowSrc
				dy := int(yStart) + y
				dstStart := dy*rowDst + int(xStart)*4
				dstEnd := dstStart + int(width)*4
				if srcEnd > len(bmpData) || dstStart < 0 || dstEnd > len(out) {
					continue
				}
				src := bmpData[srcStart:srcEnd:srcEnd]
				dst := out[dstStart:dstEnd:dstEnd]
				for x := 0; x < int(width); x++ {
					si := x * 3
					di := x * 4
					dst[di] = src[si]
					dst[di+1] = src[si+1]
					dst[di+2] = src[si+2]
					dst[di+3] = 0xFF
				}
			}
		}
		// Skip NSCodec (1) and glyph (2) subcodecs
	}
}
