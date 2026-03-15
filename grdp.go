package grdp

import (
	"fmt"
	"image"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/nakagami/grdp/plugin"
	"github.com/nakagami/grdp/plugin/drdynvc"
	"github.com/nakagami/grdp/plugin/rdpgfx"

	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/protocol/nla"
	"github.com/nakagami/grdp/protocol/pdu"
	"github.com/nakagami/grdp/protocol/sec"
	"github.com/nakagami/grdp/protocol/t125"
	"github.com/nakagami/grdp/protocol/t125/gcc"
	"github.com/nakagami/grdp/protocol/tpkt"
	"github.com/nakagami/grdp/protocol/x224"
)

type RdpClient struct {
	hostPort        string // ip:port
	width           int
	height          int
	kbdLayout       uint32
	keyboardType    uint32
	keyboardSubType uint32
	tpkt            *tpkt.TPKT
	x224            *x224.X224
	mcs             *t125.MCSClient
	sec             *sec.Client
	pdu             *pdu.Client
	channels        *plugin.Channels
	eventReady      bool
	decompressPool  sync.Pool // pools []uint8 buffers for bitmap decompression
	flipLinePool    sync.Pool // pools line-sized []uint8 buffers for bitmap vertical flip
	reconnectMu sync.Mutex
	closed      bool

	// credentials stored for reconnection
	domain   string
	user     string
	password string

	// stored callbacks for re-registration on reconnect
	onErrorFn         func(e error)
	onCloseFn         func()
	onSuccesFn        func()
	onReadyFn         func()
	onBitmapPaintFn   func([]Bitmap)
	onPointerHideFn   func()
	onPointerCachedFn func(uint16)
	onPointerUpdateFn func(uint16, uint16, uint16, uint16, uint16, uint16, []byte, []byte)
}

type Bitmap struct {
	DestLeft     int
	DestTop      int
	DestRight    int
	DestBottom   int
	Width        int
	Height       int
	BitsPerPixel int
	Data         []byte
}

func pixelToRGBA(pixel int, i int, data []byte) (r, g, b, a uint8) {
	a = 255
	switch pixel {
	case 1:
		rgb555 := core.Uint16BE(data[i], data[i+1])
		r, g, b = core.RGB555ToRGB(rgb555)
	case 2:
		rgb565 := core.Uint16BE(data[i], data[i+1])
		r, g, b = core.RGB565ToRGB(rgb565)
	case 3, 4:
		fallthrough
	default:
		r, g, b = data[i+2], data[i+1], data[i]
	}

	return
}

func (bm *Bitmap) RGBA() *image.RGBA {
	pixel := bm.BitsPerPixel
	m := image.NewRGBA(image.Rect(0, 0, bm.Width, bm.Height))
	pix := m.Pix
	dataIdx := 0
	for i := 0; i < len(pix); i += 4 {
		r, g, b, a := pixelToRGBA(pixel, dataIdx, bm.Data)
		pix[i] = r
		pix[i+1] = g
		pix[i+2] = b
		pix[i+3] = a
		dataIdx += pixel
	}
	return m
}

func NewRdpClient(host string, width, height int) *RdpClient {
	return &RdpClient{
		hostPort:        host,
		width:           width,
		height:          height,
		kbdLayout:       uint32(gcc.US),
		keyboardType:    uint32(gcc.KT_IBM_101_102_KEYS),
		keyboardSubType: 0,
		decompressPool: sync.Pool{
			New: func() any { return []uint8(nil) },
		},
		flipLinePool: sync.Pool{
			New: func() any { return []uint8(nil) },
		},
	}
}

var keyboardLayoutMap = map[string]uint32{
	"ARABIC":              uint32(gcc.ARABIC),
	"BULGARIAN":           uint32(gcc.BULGARIAN),
	"CHINESE_US_KEYBOARD": uint32(gcc.CHINESE_US_KEYBOARD),
	"CZECH":               uint32(gcc.CZECH),
	"DANISH":              uint32(gcc.DANISH),
	"GERMAN":              uint32(gcc.GERMAN),
	"GREEK":               uint32(gcc.GREEK),
	"US":                  uint32(gcc.US),
	"SPANISH":             uint32(gcc.SPANISH),
	"FINNISH":             uint32(gcc.FINNISH),
	"FRENCH":              uint32(gcc.FRENCH),
	"HEBREW":              uint32(gcc.HEBREW),
	"HUNGARIAN":           uint32(gcc.HUNGARIAN),
	"ICELANDIC":           uint32(gcc.ICELANDIC),
	"ITALIAN":             uint32(gcc.ITALIAN),
	"JAPANESE":            uint32(gcc.JAPANESE),
	"KOREAN":              uint32(gcc.KOREAN),
	"DUTCH":               uint32(gcc.DUTCH),
	"NORWEGIAN":           uint32(gcc.NORWEGIAN),
}

var keyboardTypeMap = map[string]uint32{
	"IBM_PC_XT_83_KEY": uint32(gcc.KT_IBM_PC_XT_83_KEY),
	"OLIVETTI":         uint32(gcc.KT_OLIVETTI),
	"IBM_PC_AT_84_KEY": uint32(gcc.KT_IBM_PC_AT_84_KEY),
	"IBM_101_102_KEYS": uint32(gcc.KT_IBM_101_102_KEYS),
	"NOKIA_1050":       uint32(gcc.KT_NOKIA_1050),
	"NOKIA_9140":       uint32(gcc.KT_NOKIA_9140),
	"JAPANESE":         uint32(gcc.KT_JAPANESE),
}

// SetKeyboardLayout sets the keyboard layout by name (e.g. "US", "FRENCH").
// Must be called before Login.
func (g *RdpClient) SetKeyboardLayout(layout string) {
	if v, ok := keyboardLayoutMap[strings.ToUpper(layout)]; ok {
		g.kbdLayout = v
	} else {
		slog.Warn("Unknown keyboard layout, falling back to US", "layout", layout)
		g.kbdLayout = uint32(gcc.US)
	}
}

// SetKeyboardType sets the keyboard type by name (e.g. "IBM_101_102_KEYS").
// Must be called before Login.
func (g *RdpClient) SetKeyboardType(keyboardType string) {
	if v, ok := keyboardTypeMap[strings.ToUpper(keyboardType)]; ok {
		g.keyboardType = v
	} else {
		slog.Warn("Unknown keyboard type, falling back to IBM_101_102_KEYS", "keyboardType", keyboardType)
		g.keyboardType = uint32(gcc.KT_IBM_101_102_KEYS)
	}
}

func bpp(BitsPerPixel uint16) (pixel int) {
	switch BitsPerPixel {
	case 15:
		pixel = 2

	case 16:
		pixel = 2

	case 24:
		pixel = 3

	case 32:
		pixel = 4

	default:
		slog.Error("invalid bitmap data format")
	}
	return
}

func (g *RdpClient) Login(domain string, user string, password string) error {
	slog.Info("Login", "Host", g.hostPort, "domain", domain, "user", user)

	g.domain = domain
	g.user = user
	g.password = password

	conn, err := net.DialTimeout("tcp", g.hostPort, 30*time.Second)
	if err != nil {
		return fmt.Errorf("[dial err] %v", err)
	}

	g.tpkt = tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(domain, user, password))
	g.x224 = x224.New(g.tpkt)
	g.mcs = t125.NewMCSClient(g.x224, g.kbdLayout, g.keyboardType, g.keyboardSubType)
	g.sec = sec.NewClient(g.mcs)
	g.pdu = pdu.NewClient(g.sec)
	g.channels = plugin.NewChannels(g.sec)

	g.mcs.SetClientDesktop(uint16(g.width), uint16(g.height))
	//clipboard
	//g.channels.Register(cliprdr.NewCliprdrClient())
	//g.mcs.SetClientCliprdr()

	//remote app
	//g.channels.Register(rail.NewClient())
	//g.mcs.SetClientRemoteProgram()
	//g.sec.SetAlternateShell("")

	//dvc
	dvcClient := drdynvc.NewDvcClient()
	g.channels.Register(dvcClient)
	//g.mcs.SetClientDynvcProtocol() // DIAGNOSTIC: disabled GFX EarlyCapabilityFlag to test traditional bitmap

	// RDPGFX (Graphics Pipeline) handler
	gfxHandler := rdpgfx.NewGfxHandler(func(updates []rdpgfx.BitmapUpdate) {
		if g.onBitmapPaintFn == nil {
			return
		}
		bs := make([]Bitmap, len(updates))
		for i, u := range updates {
			bs[i] = Bitmap{
				DestLeft:     u.DestLeft,
				DestTop:      u.DestTop,
				DestRight:    u.DestRight,
				DestBottom:   u.DestBottom,
				Width:        u.Width,
				Height:       u.Height,
				BitsPerPixel: u.Bpp,
				Data:         u.Data,
			}
		}
		g.onBitmapPaintFn(bs)
	})
	dvcClient.RegisterHandler(rdpgfx.ChannelName, gfxHandler)

	g.sec.SetUser(user)
	g.sec.SetPwd(password)
	g.sec.SetDomain(domain)

	g.tpkt.SetFastPathListener(g.sec)
	g.sec.SetFastPathListener(g.pdu)
	g.sec.SetChannelSender(g.mcs)
	g.channels.SetChannelSender(g.sec)
	//g.pdu.SetFastPathSender(g.tpkt)

	g.x224.SetRequestedProtocol(x224.PROTOCOL_SSL | x224.PROTOCOL_HYBRID)
	g.x224.SetUsername(user)

	err = g.x224.Connect()
	if err != nil {
		return fmt.Errorf("[x224 connect err] %v", err)
	}

	g.pdu.On("ready", func() {
		g.eventReady = true
	})

	return nil
}

func (g *RdpClient) Width() int {
	return g.width
}

func (g *RdpClient) Height() int {
	return g.height
}

func (g *RdpClient) OnError(f func(e error)) *RdpClient {
	g.onErrorFn = f
	g.pdu.On("error", f)
	return g
}

func (g *RdpClient) OnClose(f func()) *RdpClient {
	g.onCloseFn = f
	g.pdu.On("close", f)
	return g
}

func (g *RdpClient) OnSucces(f func()) *RdpClient {
	g.onSuccesFn = f
	g.pdu.On("succes", f)
	return g
}

func (g *RdpClient) OnReady(f func()) *RdpClient {
	g.onReadyFn = f
	g.pdu.On("ready", f)
	return g
}

// OnBitmap registers a callback for bitmap update events.
// For compressed bitmaps, Bitmap.Data is borrowed from an internal pool and
// is valid only for the duration of the paint call. If you need to retain
// the raw pixel data beyond paint, copy it or call bm.RGBA() inside paint.
func (g *RdpClient) OnBitmap(paint func([]Bitmap)) *RdpClient {
	g.onBitmapPaintFn = paint
	g.pdu.On("bitmap", func(rectangles []pdu.BitmapData) {
		bs := make([]Bitmap, 0, len(rectangles))
		var pooled [][]uint8 // track buffers borrowed from pool

		for _, v := range rectangles {
			data := v.BitmapDataStream
			Bpp := bpp(v.BitsPerPixel)

			if v.Flags&pdu.BITMAP_NO_PROCESSING != 0 {
				// Surface command: data is already decoded top-down BGRA
			} else if v.IsCompress() {
				buf := g.decompressPool.Get().([]uint8)
				buf = core.DecompressInto(v.BitmapDataStream, buf, int(v.Width), int(v.Height), Bpp)
				data = buf
				pooled = append(pooled, buf)
			} else {
				// Uncompressed bitmaps are bottom-up; flip to top-down.
				stride := int(v.Width) * Bpp
				h := int(v.Height)
				tmp := g.flipLinePool.Get().([]byte)
				if cap(tmp) < stride {
					tmp = make([]byte, stride)
				} else {
					tmp = tmp[:stride]
				}
				for y := 0; y < h/2; y++ {
					top := y * stride
					bot := (h - 1 - y) * stride
					copy(tmp, data[top:top+stride])
					copy(data[top:top+stride], data[bot:bot+stride])
					copy(data[bot:bot+stride], tmp)
				}
				g.flipLinePool.Put(tmp[:cap(tmp)])
			}

			b := Bitmap{int(v.DestLeft), int(v.DestTop), int(v.DestRight), int(v.DestBottom),
				int(v.Width), int(v.Height), Bpp, data}
			bs = append(bs, b)
		}
		paint(bs)

		for _, buf := range pooled {
			g.decompressPool.Put(buf[:cap(buf)])
		}
	})
	return g
}

func (g *RdpClient) OnPointerHide(f func()) *RdpClient {
	g.onPointerHideFn = f
	g.pdu.On("pointer_hide", f)
	return g
}

func (g *RdpClient) OnPointerCached(f func(uint16)) *RdpClient {
	g.onPointerCachedFn = f
	g.pdu.On("pointer_cached", f)
	return g
}

func (g *RdpClient) OnPointerUpdate(f func(uint16, uint16, uint16, uint16, uint16, uint16, []byte, []byte)) *RdpClient {
	g.onPointerUpdateFn = f
	g.pdu.On("pointer_update", func(p *pdu.FastPathUpdatePointerPDU) {
		f(p.CacheIdx, p.XorBpp, p.X, p.Y, p.Width, p.Height, p.Mask, p.Data)
	})
	return g
}

func (g *RdpClient) KeyUp(sc int) {
	slog.Debug("KeyUp", "sc", sc)

	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(sc)
	p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) KeyDown(sc int) {
	slog.Debug("KeyDown", "sc", sc)

	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(sc)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseMove(x, y int) {
	//slog.Debug("MouseMove", "x", x, "y", y)
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_MOVE
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseWheel(scroll int) {
	slog.Debug("MouseWheel")
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_WHEEL
	if scroll < 0 {
		p.PointerFlags |= pdu.PTRFLAGS_WHEEL_NEGATIVE
	}
	var ts uint8 = uint8(scroll)
	p.PointerFlags |= pdu.WheelRotationMask & uint16(ts)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseUp(button int, x, y int) {
	slog.Debug("MouseUp", "x", x, "y", y, "button", button)
	p := &pdu.PointerEvent{}

	switch button {
	case 0:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON1
	case 2:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON2
	case 1:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON3
	default:
		p.PointerFlags |= pdu.PTRFLAGS_MOVE
	}

	p.XPos = uint16(x)
	p.YPos = uint16(y)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseDown(button int, x, y int) {
	slog.Debug("MouseDown", "x", x, "y", y, "button", button)
	p := &pdu.PointerEvent{}

	p.PointerFlags |= pdu.PTRFLAGS_DOWN

	switch button {
	case 0:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON1
	case 2:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON2
	case 1:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON3
	default:
		p.PointerFlags |= pdu.PTRFLAGS_MOVE
	}

	p.XPos = uint16(x)
	p.YPos = uint16(y)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

// Reconnect closes the current RDP session and re-establishes a new connection
// with the specified screen dimensions. All previously registered callbacks are
// automatically re-registered on the new session.
// This is intended to be called when the GUI window is resized.
func (g *RdpClient) Reconnect(width, height int) error {
	g.reconnectMu.Lock()
	defer g.reconnectMu.Unlock()

	if g.closed {
		return fmt.Errorf("client is closed")
	}

	slog.Info("Reconnect", "width", width, "height", height)
	g.closeLocked()
	g.width = width
	g.height = height
	g.eventReady = false

	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Exponential backoff: 1s, 2s, 4s — gives the server time to
		// tear down the previous session before we reconnect.
		delay := time.Duration(1<<uint(attempt-1)) * time.Second
		slog.Info("Reconnect: waiting before attempt", "attempt", attempt, "delay", delay)
		time.Sleep(delay)

		err := g.Login(g.domain, g.user, g.password)
		if err != nil {
			slog.Warn("Reconnect: login failed", "attempt", attempt, "err", err)
			if attempt < maxRetries {
				g.closeLocked()
				continue
			}
			return fmt.Errorf("[reconnect err] %v", err)
		}

		// Re-register user callbacks on the new protocol objects so that
		// the "ready" / "error" events reach the caller's handlers.
		g.reregisterCallbacks()

		// Wait for the full RDP handshake (MCS, SEC, PDU capability
		// exchange) to complete before declaring success.
		result := make(chan error, 1)
		g.pdu.Once("ready", func() {
			select {
			case result <- nil:
			default:
			}
		})
		g.pdu.Once("error", func(err error) {
			select {
			case result <- err:
			default:
			}
		})

		select {
		case err := <-result:
			if err == nil {
				slog.Info("Reconnect: succeeded", "attempt", attempt)
				return nil
			}
			slog.Warn("Reconnect: handshake failed", "attempt", attempt, "err", err)
			g.closeLocked()
			if attempt < maxRetries {
				continue
			}
			return fmt.Errorf("[reconnect handshake err] %v", err)
		case <-time.After(30 * time.Second):
			slog.Warn("Reconnect: timed out", "attempt", attempt)
			g.closeLocked()
			if attempt < maxRetries {
				continue
			}
			return fmt.Errorf("[reconnect timeout]")
		}
	}

	return fmt.Errorf("[reconnect failed after %d attempts]", maxRetries)
}

func (g *RdpClient) reregisterCallbacks() {
	if g.onErrorFn != nil {
		g.OnError(g.onErrorFn)
	}
	if g.onCloseFn != nil {
		g.OnClose(g.onCloseFn)
	}
	if g.onSuccesFn != nil {
		g.OnSucces(g.onSuccesFn)
	}
	if g.onReadyFn != nil {
		g.OnReady(g.onReadyFn)
	}
	if g.onBitmapPaintFn != nil {
		g.OnBitmap(g.onBitmapPaintFn)
	}
	if g.onPointerHideFn != nil {
		g.OnPointerHide(g.onPointerHideFn)
	}
	if g.onPointerCachedFn != nil {
		g.OnPointerCached(g.onPointerCachedFn)
	}
	if g.onPointerUpdateFn != nil {
		g.OnPointerUpdate(g.onPointerUpdateFn)
	}
}

// closeLocked closes the underlying transport. Caller must hold reconnectMu.
func (g *RdpClient) closeLocked() {
	if g.tpkt != nil {
		g.tpkt.Close()
	}
}

func (g *RdpClient) Close() {
	g.reconnectMu.Lock()
	defer g.reconnectMu.Unlock()
	slog.Debug("Close()")
	g.closed = true
	g.closeLocked()
}
