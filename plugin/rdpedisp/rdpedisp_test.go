package rdpedisp

import (
	"encoding/binary"
	"testing"
)

func TestRdpedispQueuesBeforeCaps(t *testing.T) {
	h := NewHandler(1920, 1080)
	var sent [][]byte
	h.SetSendFunc(func(data []byte) {
		copied := make([]byte, len(data))
		copy(copied, data)
		sent = append(sent, copied)
	})

	// OnChannelCreated should NOT send anything
	h.OnChannelCreated()
	if len(sent) != 0 {
		t.Fatalf("expected 0 packets before CAPS, got %d", len(sent))
	}

	// Server sends CAPS PDU
	capsData := make([]byte, 20)
	binary.LittleEndian.PutUint32(capsData[0:], pduTypeCaps)
	binary.LittleEndian.PutUint32(capsData[4:], 20)
	binary.LittleEndian.PutUint32(capsData[8:], 16) // MaxNumMonitors = 16
	binary.LittleEndian.PutUint32(capsData[12:], 4096)
	binary.LittleEndian.PutUint32(capsData[16:], 2048)

	h.Process(capsData)

	if len(sent) != 1 {
		t.Fatalf("expected 1 queued packet sent after CAPS, got %d", len(sent))
	}

	pdu := sent[0]
	if len(pdu) < 16+monitorLayoutSize {
		t.Fatalf("invalid pdu length: %d", len(pdu))
	}
	pduType := binary.LittleEndian.Uint32(pdu[0:])
	if pduType != pduTypeMonitorLayout {
		t.Fatalf("expected pduTypeMonitorLayout (2), got %d", pduType)
	}
	w := binary.LittleEndian.Uint32(pdu[16+12:])
	hVal := binary.LittleEndian.Uint32(pdu[16+16:])
	if w != 1920 || hVal != 1080 {
		t.Fatalf("expected 1920x1080, got %dx%d", w, hVal)
	}

	// Subsequent SendMonitorLayout sends immediately
	h.SendMonitorLayout([]Monitor{
		{
			Flags:  MonitorFlagPrimary,
			Width:  1280,
			Height: 720,
		},
	})

	if len(sent) != 2 {
		t.Fatalf("expected 2 packets, got %d", len(sent))
	}
}
