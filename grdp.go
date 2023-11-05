package grdp

import (
	"errors"
	"fmt"
	"log/slog"
	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/protocol/nla"
	"github.com/nakagami/grdp/protocol/pdu"
	"github.com/nakagami/grdp/protocol/sec"
	"github.com/nakagami/grdp/protocol/t125"
	"github.com/nakagami/grdp/protocol/tpkt"
	"github.com/nakagami/grdp/protocol/x224"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type Client struct {
	Host string // ip:port
	tpkt *tpkt.TPKT
	x224 *x224.X224
	mcs  *t125.MCSClient
	sec  *sec.Client
	pdu  *pdu.Client
}

func NewClient(host string) *Client {
	return &Client{
		Host: host,
	}
}

func (g *Client) Login(user, pwd string) error {
	conn, err := net.DialTimeout("tcp", g.Host, 3*time.Second)
	if err != nil {
		return errors.New(fmt.Sprintf("[dial err] %v", err))
	}
	defer conn.Close()

	domain := strings.Split(g.Host, ":")[0]

	g.tpkt = tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(domain, user, pwd))
	g.x224 = x224.New(g.tpkt)
	g.mcs = t125.NewMCSClient(g.x224)
	g.sec = sec.NewClient(g.mcs)
	g.pdu = pdu.NewClient(g.sec)

	g.sec.SetUser(user)
	g.sec.SetPwd(pwd)
	g.sec.SetDomain(domain)

	g.tpkt.SetFastPathListener(g.pdu)
	g.pdu.SetFastPathSender(g.tpkt)

	g.x224.SetRequestedProtocol(x224.PROTOCOL_SSL | x224.PROTOCOL_HYBRID)

	err = g.x224.Connect()
	if err != nil {
		return errors.New(fmt.Sprintf("[x224 connect err] %v", err))
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)

	g.pdu.On("error", func(e error) {
		slog.Error("%v", e)
		wg.Done()
	}).On("close", func() {
		err = errors.New("close")
		slog.Info("on close")
		wg.Done()
	}).On("success", func() {
		err = nil
		slog.Info("on success")
		wg.Done()
	}).On("ready", func() {
		slog.Info("on ready")
	}).On("update", func(rectangles []pdu.BitmapData) {
		slog.Info("on update")
	})

	wg.Wait()
	return err
}

func (g *Client) OnError(f func(e error)) {
	g.pdu.On("error", f)
}
func (g *Client) OnClose(f func()) {
	g.pdu.On("close", f)
}
func (g *Client) OnSuccess(f func()) {
	g.pdu.On("success", f)
}
func (g *Client) OnReady(f func()) {
	g.pdu.On("ready", f)
}
func (g *Client) OnUpdate(f func([]pdu.BitmapData)) {
	g.pdu.On("update", f)
}

func (g *Client) Close() {
	if g.tpkt != nil {
		g.tpkt.Close()
	}
}
