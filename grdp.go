package grdp

import (
	"fmt"
	"net"
	"time"

	"github.com/nakagami/grdp/plugin"

	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/glog"
	"github.com/nakagami/grdp/protocol/nla"
	"github.com/nakagami/grdp/protocol/pdu"
	"github.com/nakagami/grdp/protocol/sec"
	"github.com/nakagami/grdp/protocol/t125"
	"github.com/nakagami/grdp/protocol/tpkt"
	"github.com/nakagami/grdp/protocol/x224"
)

type RdpClient struct {
	Host     string // ip:port
	Width    int
	Height   int
	Domain   string
	Username string
	Password string
	tpkt     *tpkt.TPKT
	x224     *x224.X224
	mcs      *t125.MCSClient
	sec      *sec.Client
	pdu      *pdu.Client
	channels *plugin.Channels
}

func NewRdpClient(host string, width, height int, domain, username, password string) *RdpClient {
	return &RdpClient{
		Host:     host,
		Width:    width,
		Height:   height,
		Domain:   domain,
		Username: username,
		Password: password,
	}
}
func (g *RdpClient) SetRequestedProtocol(p uint32) {
	g.x224.SetRequestedProtocol(p)
}

func Bpp(BitsPerPixel uint16) (pixel int) {
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
        glog.Error("invalid bitmap data format")
    }
    return
}

func BitmapDecompress(bitmap *pdu.BitmapData) []byte {
	return core.Decompress(bitmap.BitmapDataStream, int(bitmap.Width), int(bitmap.Height), Bpp(bitmap.BitsPerPixel))
}

func (g *RdpClient) Login() error {
	domain, user, pwd := g.Domain, g.Username, g.Password
	glog.Info("Connect:", g.Host, "with", domain+"\\"+user, ":", pwd)
	conn, err := net.DialTimeout("tcp", g.Host, 3*time.Second)
	if err != nil {
		return fmt.Errorf("[dial err] %v", err)
	}

	g.tpkt = tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(domain, user, pwd))
	g.x224 = x224.New(g.tpkt)
	g.mcs = t125.NewMCSClient(g.x224)
	g.sec = sec.NewClient(g.mcs)
	g.pdu = pdu.NewClient(g.sec)
	g.channels = plugin.NewChannels(g.sec)

	g.mcs.SetClientDesktop(uint16(g.Width), uint16(g.Height))
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
	g.sec.SetPwd(pwd)
	g.sec.SetDomain(domain)

	g.tpkt.SetFastPathListener(g.sec)
	g.sec.SetFastPathListener(g.pdu)
	g.sec.SetChannelSender(g.mcs)
	g.channels.SetChannelSender(g.sec)
	//g.pdu.SetFastPathSender(g.tpkt)

	//g.x224.SetRequestedProtocol(x224.PROTOCOL_RDP)
	g.x224.SetRequestedProtocol(x224.PROTOCOL_SSL | x224.PROTOCOL_HYBRID)

	err = g.x224.Connect()
	if err != nil {
		return fmt.Errorf("[x224 connect err] %v", err)
	}
	return nil
}

func (g *RdpClient) KeyUp(sc int, name string) {
	glog.Debugf("KeyUp: 0x%x, name: %s", sc, name)

	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(sc)
	p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}
func (g *RdpClient) KeyDown(sc int, name string) {
	glog.Debugf("KeyDown: 0x%x, name: %s", sc, name)

	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(sc)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseMove(x, y int) {
	glog.Debug("MouseMove", x, ":", y)
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_MOVE
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseWheel(scroll, x, y int) {
	glog.Info("MouseWheel", x, ":", y)
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_WHEEL
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (g *RdpClient) MouseUp(button int, x, y int) {
	glog.Debug("MouseUp", x, ":", y, ":", button)
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
	glog.Info("MouseDown:", x, ":", y, ":", button)
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
	if g != nil && g.tpkt != nil {
		g.tpkt.Close()
	}
}
