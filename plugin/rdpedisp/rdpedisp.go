// Package rdpedisp implements the RDP Display Update Virtual Channel
// (MS-RDPEDISP), which allows the client to request a resolution change
// while connected.  The channel name is:
//
//	"Microsoft::Windows::RDS::DisplayControl"
//
// Typical usage:
//
//  1. Register the handler with the DVC client before connecting.
//  2. After the session is established, call SendMonitorLayout to request
//     a new resolution.  The server will reshape the desktop and send a
//     fresh RDPGFX ResetGraphics command.
package rdpedisp

import (
	"encoding/binary"
	"log/slog"
	"sync"
)

// ChannelName is the well-known DVC name for the Display Update channel.
const ChannelName = "Microsoft::Windows::RDS::DisplayControl"

// PDU types (MS-RDPEDISP 2.2.1.2.1)
const (
	pduTypeCaps          = 0x00000005
	pduTypeMonitorLayout = 0x00000002
)

// DISPLAYCONTROL_MONITOR_PRIMARY marks the primary monitor.
const MonitorFlagPrimary = uint32(0x00000001)

// monitorLayoutSize is the fixed per-monitor record size required by the spec.
const monitorLayoutSize = 40

// Monitor describes a single monitor in a MONITOR_LAYOUT PDU.
// Width and Height must each be at least 200 and Width must be even.
// Set PhysicalWidth/Height to 0 when the physical dimensions are unknown.
// Orientation: 0=landscape (normal), 90, 180, 270.
// DesktopScaleFactor: one of 100, 125, 150, 175, 200 (use 100 if unsure).
// DeviceScaleFactor:  one of 100, 140, 180           (use 100 if unsure).
type Monitor struct {
	Flags              uint32
	Left               int32
	Top                int32
	Width              uint32
	Height             uint32
	PhysicalWidth      uint32
	PhysicalHeight     uint32
	Orientation        uint32
	DesktopScaleFactor uint32
	DeviceScaleFactor  uint32
}

// Handler is the DVC handler for the Display Update channel.
// It implements the drdynvc.DvcChannelHandler interface and the optional
// SetSendFunc / OnChannelCreated extension interfaces.
type Handler struct {
	mu              sync.Mutex
	send            func([]byte)
	capsReceived    bool
	maxMonitors     uint32
	pendingMonitors []Monitor
}

// NewHandler returns a new Handler.
// width and height are the desired initial desktop dimensions; when both are
// non-zero the handler queues an initial layout change that is sent as soon as
// the server advertises its capabilities (MS-RDPEDISP CAPS PDU), prompting
// servers such as GNOME Remote Desktop (headless) to resize.
func NewHandler(width, height uint32) *Handler {
	h := &Handler{}
	if width > 0 && height > 0 {
		h.pendingMonitors = []Monitor{
			{
				Flags:              MonitorFlagPrimary,
				Left:               0,
				Top:                0,
				Width:              width,
				Height:             height,
				PhysicalWidth:      0,
				PhysicalHeight:     0,
				Orientation:        0,
				DesktopScaleFactor: 100,
				DeviceScaleFactor:  100,
			},
		}
	}
	return h
}

// SetSendFunc is called by the DVC client to provide a write-back function.
// Required by the drdynvc channel plumbing.
func (h *Handler) SetSendFunc(f func([]byte)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.send = f
}

// OnChannelCreated is called by the DVC client after the CREATE_RSP has been sent.
func (h *Handler) OnChannelCreated() {
	slog.Debug("rdpedisp: channel created")
}

// Process handles incoming data from the server (CAPS PDU, etc.).
func (h *Handler) Process(data []byte) {
	if len(data) < 8 {
		return
	}
	pduType := binary.LittleEndian.Uint32(data[0:4])
	switch pduType {
	case pduTypeCaps:
		if len(data) >= 20 {
			maxMonitors := binary.LittleEndian.Uint32(data[8:12])
			slog.Debug("rdpedisp: server CAPS", "maxMonitors", maxMonitors)
			h.mu.Lock()
			h.capsReceived = true
			h.maxMonitors = maxMonitors
			pending := h.pendingMonitors
			h.pendingMonitors = nil
			h.mu.Unlock()

			if len(pending) > 0 {
				h.SendMonitorLayout(pending)
			}
		}
	default:
		slog.Debug("rdpedisp: unknown PDU type", "type", pduType)
	}
}

// SendMonitorLayout sends a DISPLAYCONTROL_MONITOR_LAYOUT_PDU to the server,
// requesting the given monitor configuration.  Per MS-RDPEDISP 3.2.5.1, the
// client MUST NOT send a layout PDU before receiving the server CAPS PDU; if
// called before CAPS is received, the layout is queued and sent automatically
// when CAPS arrives.
//
// The server will apply the new layout and—if using the RDPGFX pipeline—send
// a ResetGraphics command that resets surface dimensions to match.
func (h *Handler) SendMonitorLayout(monitors []Monitor) {
	h.mu.Lock()
	if !h.capsReceived {
		slog.Debug("rdpedisp: queuing MonitorLayout (waiting for server CAPS)", "numMonitors", len(monitors))
		h.pendingMonitors = monitors
		h.mu.Unlock()
		return
	}
	send := h.send
	h.mu.Unlock()

	if send == nil {
		slog.Warn("rdpedisp: SendMonitorLayout: channel not open")
		return
	}

	numMonitors := uint32(len(monitors))
	// PDU layout: 8-byte DISPLAYCONTROL_HEADER +
	//             4 bytes MonitorLayoutSize +
	//             4 bytes NumMonitors +
	//             numMonitors * monitorLayoutSize bytes
	pduLen := uint32(8 + 4 + 4 + int(numMonitors)*monitorLayoutSize)
	pdu := make([]byte, pduLen)

	binary.LittleEndian.PutUint32(pdu[0:], pduTypeMonitorLayout)
	binary.LittleEndian.PutUint32(pdu[4:], pduLen)
	binary.LittleEndian.PutUint32(pdu[8:], monitorLayoutSize) // fixed record size
	binary.LittleEndian.PutUint32(pdu[12:], numMonitors)

	off := 16
	for _, m := range monitors {
		binary.LittleEndian.PutUint32(pdu[off+0:], m.Flags)
		binary.LittleEndian.PutUint32(pdu[off+4:], uint32(m.Left))
		binary.LittleEndian.PutUint32(pdu[off+8:], uint32(m.Top))
		binary.LittleEndian.PutUint32(pdu[off+12:], m.Width)
		binary.LittleEndian.PutUint32(pdu[off+16:], m.Height)
		binary.LittleEndian.PutUint32(pdu[off+20:], m.PhysicalWidth)
		binary.LittleEndian.PutUint32(pdu[off+24:], m.PhysicalHeight)
		binary.LittleEndian.PutUint32(pdu[off+28:], m.Orientation)
		binary.LittleEndian.PutUint32(pdu[off+32:], m.DesktopScaleFactor)
		binary.LittleEndian.PutUint32(pdu[off+36:], m.DeviceScaleFactor)
		off += monitorLayoutSize
	}

	slog.Debug("rdpedisp: sending MonitorLayout", "numMonitors", numMonitors)
	send(pdu)
}
