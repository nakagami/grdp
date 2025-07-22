package grdp

import (
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"net"

	"github.com/nakagami/grdp/plugin"

	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/protocol/nla"
	"github.com/nakagami/grdp/protocol/pdu"
	"github.com/nakagami/grdp/protocol/sec"
	"github.com/nakagami/grdp/protocol/t125"
	"github.com/nakagami/grdp/protocol/t125/gcc"
	"github.com/nakagami/grdp/protocol/tpkt"
	"github.com/nakagami/grdp/protocol/x224"
)

var (
	KbdLayout       uint32 = gcc.US
	KeyboardType    uint32 = gcc.KT_IBM_101_102_KEYS
	KeyboardSubType uint32 = 0
)

type RdpClient struct {
	hostPort   string // ip:port
	width      int
	height     int
	tpkt       *tpkt.TPKT
	x224       *x224.X224
	mcs        *t125.MCSClient
	sec        *sec.Client
	pdu        *pdu.Client
	channels   *plugin.Channels
	eventReady bool
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
	i := 0
	pixel := bm.BitsPerPixel
	m := image.NewRGBA(image.Rect(0, 0, bm.Width, bm.Height))
	for y := 0; y < bm.Height; y++ {
		for x := 0; x < bm.Width; x++ {
			r, g, b, a := pixelToRGBA(pixel, i, bm.Data)
			c := color.RGBA{r, g, b, a}
			i += pixel
			m.Set(x, y, c)
		}
	}
	return m
}

func NewRdpClient(host string, width, height int) *RdpClient {
	return &RdpClient{
		hostPort: host,
		width:    width,
		height:   height,
	}
}

func bpp(BitsPerPixel uint16) (pixel int) {
	switch BitsPerPixel {
	case 15:
		pixel = 1

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
	conn, err := net.Dial("tcp", g.hostPort)
	if err != nil {
		return fmt.Errorf("[dial err] %v", err)
	}

	g.tpkt = tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(domain, user, password))
	g.x224 = x224.New(g.tpkt)
	g.mcs = t125.NewMCSClient(g.x224, KbdLayout, KeyboardType, KeyboardSubType)
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
	//g.channels.Register(drdynvc.NewDvcClient())

	g.sec.SetUser(user)
	g.sec.SetPwd(password)
	g.sec.SetDomain(domain)

	g.tpkt.SetFastPathListener(g.sec)
	g.sec.SetFastPathListener(g.pdu)
	g.sec.SetChannelSender(g.mcs)
	g.channels.SetChannelSender(g.sec)
	//g.pdu.SetFastPathSender(g.tpkt)

	g.x224.SetRequestedProtocol(x224.PROTOCOL_SSL | x224.PROTOCOL_HYBRID)

	err = g.x224.Connect()
	if err != nil {
		return fmt.Errorf("[x224 connect err] %v", err)
	}

	g.OnReady(func() {
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
	g.pdu.On("error", f)
	return g
}

func (g *RdpClient) OnClose(f func()) *RdpClient {
	g.pdu.On("close", f)
	return g
}

func (g *RdpClient) OnSucces(f func()) *RdpClient {
	g.pdu.On("succes", f)
	return g
}

func (g *RdpClient) OnReady(f func()) *RdpClient {
	g.pdu.On("ready", f)
	return g
}

func (g *RdpClient) OnBitmap(paint func([]Bitmap)) *RdpClient {
	g.pdu.On("bitmap", func(rectangles []pdu.BitmapData) {
		if !g.eventReady {
			return
		}
		bs := make([]Bitmap, 0, 50)
		for _, v := range rectangles {
			IsCompress := v.IsCompress()
			data := v.BitmapDataStream
			if IsCompress {
				data = core.Decompress(v.BitmapDataStream, int(v.Width), int(v.Height), bpp(v.BitsPerPixel))
				IsCompress = false
			}

			b := Bitmap{int(v.DestLeft), int(v.DestTop), int(v.DestRight), int(v.DestBottom),
				int(v.Width), int(v.Height), bpp(v.BitsPerPixel), data}
			bs = append(bs, b)
		}
		paint(bs)
	})
	return g
}

func (g *RdpClient) OnPointerHide(f func()) *RdpClient {
	g.pdu.On("pointer_hide", f)
	return g
}

func (g *RdpClient) OnPointerCached(f func(uint16)) *RdpClient {
	g.pdu.On("pointer_cached", f)
	return g
}

func (g *RdpClient) OnPointerUpdate(f func(uint16, uint16, uint16, uint16, uint16, uint16, []byte, []byte)) *RdpClient {
	g.pdu.On("pointer_update", func(p *pdu.FastPathUpdatePointerPDU) {
		f(p.CacheIdx, p.XorBpp, p.X, p.Y, p.Width, p.Height, p.Mask, p.Data)
	})
	return g
}

func (g *RdpClient) KeyUp(sc int) {
	if !g.eventReady {
		return
	}
	slog.Debug("KeyUp", "sc", sc)

	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(sc)
	p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) KeyDown(sc int) {
	if !g.eventReady {
		return
	}
	slog.Debug("KeyDown", "sc", sc)

	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(sc)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseMove(x, y int) {
	if !g.eventReady {
		return
	}
	//slog.Debug("MouseMove", "x", x, "y", y)
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_MOVE
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseWheel(scroll int) {
	if !g.eventReady {
		return
	}
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
	if !g.eventReady {
		return
	}
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
	if !g.eventReady {
		return
	}
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

func (g *RdpClient) Close() {
	slog.Debug("Close()")
	if !g.eventReady {
		return
	}
	if g != nil && g.tpkt != nil {
		g.tpkt.Close()
	}
}
