package rdpgfx

import (
	"encoding/hex"
	"log/slog"

	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/plugin"
)

const (
	ChannelName = plugin.RDPGFX_DVC_CHANNEL_NAME
)

type gfxClient struct {
}

func (c *gfxClient) Send(s []byte) (int, error) {
	slog.Debug("len:", len(s), "data:", hex.EncodeToString(s))
	name, _ := c.GetType()
	return c.w.SendToChannel(name, s)
}
func (c *gfxClient) Sender(f core.ChannelSender) {
	c.w = f
}
func (c *gfxClient) GetType() (string, uint32) {
	return ChannelName, 0
}
