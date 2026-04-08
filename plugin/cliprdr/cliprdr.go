//go:build windows

package cliprdr

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/lunixbochs/struc"

	"github.com/nakagami/grdp/core"
)

type CliprdrClient struct {
	w                     core.ChannelSender
	useLongFormatNames    bool
	streamFileClipEnabled bool
	fileClipNoFilePaths   bool
	canLockClipData       bool
	hasHugeFileSupport    bool
	formatIdMap           map[uint32]uint32
	Files                 []FileDescriptor
	reply                 chan []byte
	Control
}

func NewCliprdrClient() *CliprdrClient {
	c := &CliprdrClient{
		formatIdMap: make(map[uint32]uint32, 20),
		Files:       make([]FileDescriptor, 0, 20),
		reply:       make(chan []byte, 100),
	}

	go ClipWatcher(c)

	return c
}

func (c *CliprdrClient) Sender(f core.ChannelSender) {
	c.w = f
}
func (c *CliprdrClient) GetType() (string, uint32) {
	return ChannelName, ChannelOption
}

func (c *CliprdrClient) Process(s []byte) {
	r := bytes.NewReader(s)

	msgType, _ := core.ReadUint16LE(r)
	flag, _ := core.ReadUint16LE(r)
	length, _ := core.ReadUInt32LE(r)
	slog.Debug(fmt.Sprintf("cliprdr: type=0x%x flag=%d length=%d, all=%d", msgType, flag, length, r.Len()))

	b, _ := core.ReadBytes(int(length), r)

	switch msgType {
	case CB_CLIP_CAPS:
		slog.Debug("CB_CLIP_CAPS")
		c.processClipCaps(b)

	case CB_MONITOR_READY:
		slog.Debug("CB_MONITOR_READY")
		c.processMonitorReady(b)

	case CB_FORMAT_LIST:
		slog.Debug("CB_FORMAT_LIST")
		c.processFormatList(b)

	case CB_FORMAT_LIST_RESPONSE:
		slog.Debug("CB_FORMAT_LIST_RESPONSE")
		c.processFormatListResponse(flag, b)

	case CB_FORMAT_DATA_REQUEST:
		slog.Debug("CB_FORMAT_DATA_REQUEST")
		c.processFormatDataRequest(b)

	case CB_FORMAT_DATA_RESPONSE:
		slog.Debug("CB_FORMAT_DATA_RESPONSE")
		c.processFormatDataResponse(flag, b)

	case CB_FILECONTENTS_REQUEST:
		slog.Debug("CB_FILECONTENTS_REQUEST")
		c.processFileContentsRequest(b)

	case CB_FILECONTENTS_RESPONSE:
		slog.Debug("CB_FILECONTENTS_RESPONSE")
		c.processFileContentsResponse(flag, b)

	case CB_LOCK_CLIPDATA:
		slog.Debug("CB_LOCK_CLIPDATA")
		c.processLockClipData(b)

	case CB_UNLOCK_CLIPDATA:
		slog.Debug("CB_UNLOCK_CLIPDATA")
		c.processUnlockClipData(b)

	default:
		slog.Error(fmt.Sprintf("type 0x%x not supported", msgType))
	}
}
func (c *CliprdrClient) processClipCaps(b []byte) {
	r := bytes.NewReader(b)
	var cp CliprdrCapabilitiesPDU
	err := struc.Unpack(r, &cp)
	if err != nil {
		slog.Error("err", err)
		return
	}
	slog.Debug(fmt.Sprintf("Capabilities:%+v", cp))
	c.useLongFormatNames = cp.CapabilitySets[0].GeneralFlags&CB_USE_LONG_FORMAT_NAMES != 0
	c.streamFileClipEnabled = cp.CapabilitySets[0].GeneralFlags&CB_STREAM_FILECLIP_ENABLED != 0
	c.fileClipNoFilePaths = cp.CapabilitySets[0].GeneralFlags&CB_FILECLIP_NO_FILE_PATHS != 0
	c.canLockClipData = cp.CapabilitySets[0].GeneralFlags&CB_CAN_LOCK_CLIPDATA != 0
	c.hasHugeFileSupport = cp.CapabilitySets[0].GeneralFlags&CB_HUGE_FILE_SUPPORT_ENABLED != 0
	slog.Debug("UseLongFormatNames:", c.useLongFormatNames)
	slog.Debug("StreamFileClipEnabled:", c.streamFileClipEnabled)
	slog.Debug("FileClipNoFilePaths:", c.fileClipNoFilePaths)
	slog.Debug("CanLockClipData:", c.canLockClipData)
	slog.Debug("HasHugeFileSupport:", c.hasHugeFileSupport)
}

func (c *CliprdrClient) processMonitorReady(b []byte) {
	c.sendClientCapabilitiesPDU()
	c.sendFormatListPDU()
}

func (c *CliprdrClient) processFormatList(b []byte) {
	c.withOpenClipboard(func() {
		if !EmptyClipboard() {
			slog.Error("EmptyClipboard failed")
		}
	})
	fl, hasFile := c.readForamtList(b)
	slog.Debug("numFormats:", fl.NumFormats)

	if hasFile {
		c.SendCliprdrMessage()
	} else {
		c.withOpenClipboard(func() {
			if !EmptyClipboard() {
				slog.Error("EmptyClipboard failed")
			}
			for i := range c.formatIdMap {
				slog.Debug("i:", i)
				SetClipboardData(i, 0)
			}
		})

	}

	c.sendFormatListResponse(CB_RESPONSE_OK)
}
func (c *CliprdrClient) processFormatListResponse(flag uint16, b []byte) {
	if flag != CB_RESPONSE_OK {
		slog.Error("Format List Response Failed")
		return
	}
	slog.Error("Format List Response OK")
}
func getFilesDescriptor(name string) (FileDescriptor, error) {
	var fd FileDescriptor
	fd.Flags = FD_ATTRIBUTES | FD_FILESIZE | FD_WRITESTIME | FD_PROGRESSUI
	f, e := os.Stat(name)
	if e != nil {
		slog.Error(e.Error())
		return fd, e
	}
	fd.FileAttributes, fd.LastWriteTime,
		fd.FileSizeHigh, fd.FileSizeLow = GetFileInfo(f.Sys())
	fd.FileName = core.UnicodeEncode(name)

	return fd, nil
}
func (c *CliprdrClient) processFormatDataRequest(b []byte) {
	r := bytes.NewReader(b)
	requestId, _ := core.ReadUInt32LE(r)

	buff := &bytes.Buffer{}
	if requestId == RegisterClipboardFormat(CFSTR_FILEDESCRIPTORW) {
		fs := GetFileNames()
		core.WriteUInt32LE(uint32(len(fs)), buff)
		c.Files = c.Files[:0]
		for _, v := range fs {
			slog.Debug("Name:", v)
			f, _ := getFilesDescriptor(v)
			buff.Write(f.serialize())
			for i := 0; i < 8; i++ {
				buff.WriteByte(0)
			}
			c.Files = append(c.Files, f)

		}
	} else {
		c.withOpenClipboard(func() {
			data := GetClipboardData(requestId)
			slog.Debug("data:", data)
			buff.Write(core.UnicodeEncode(data))
			buff.Write([]byte{0, 0})
		})
	}

	c.sendFormatDataResponse(buff.Bytes())
}
func (c *CliprdrClient) processFormatDataResponse(flag uint16, b []byte) {
	if flag != CB_RESPONSE_OK {
		slog.Error("Format Data Response Failed")
	}
	c.reply <- b
}

func (c *CliprdrClient) processFileContentsRequest(b []byte) {
	r := bytes.NewReader(b)
	var req CliprdrFileContentsRequest
	struc.Unpack(r, &req)
	if len(c.Files) <= int(req.Lindex) {
		slog.Error("No found file:", req.Lindex)
		c.sendFormatContentsResponse(req.StreamId, []byte{})
		return
	}
	buff := &bytes.Buffer{}
	f := c.Files[req.Lindex]
	if req.DwFlags == FILECONTENTS_SIZE {
		core.WriteUInt32LE(f.FileSizeLow, buff)
		core.WriteUInt32LE(f.FileSizeHigh, buff)
		c.sendFormatContentsResponse(req.StreamId, buff.Bytes())
	} else if req.DwFlags == FILECONTENTS_RANGE {
		name := core.UnicodeDecode(f.FileName)
		fi, err := os.Open(name)
		if err != nil {
			slog.Error(err.Error())
			return
		}
		defer fi.Close()
		data := make([]byte, req.CbRequested)
		n, _ := fi.ReadAt(data, int64(f.FileSizeHigh))
		c.sendFormatContentsResponse(req.StreamId, data[:n])
	}
}
func (c *CliprdrClient) processFileContentsResponse(flag uint16, b []byte) {
	if flag != CB_RESPONSE_OK {
		slog.Error("File Contents Response Failed")
	}
	var resp CliprdrFileContentsResponse
	resp.Unpack(b)
	slog.Debug("Get File Contents Response:", resp.StreamId, resp.CbRequested)
	c.reply <- resp.RequestedData
}
func (c *CliprdrClient) processLockClipData(b []byte) {
	r := bytes.NewReader(b)
	var l CliprdrCtrlClipboardData
	l.ClipDataId, _ = core.ReadUInt32LE(r)
}
func (c *CliprdrClient) processUnlockClipData(b []byte) {
	r := bytes.NewReader(b)
	var l CliprdrCtrlClipboardData
	l.ClipDataId, _ = core.ReadUInt32LE(r)

}

func (c *CliprdrClient) sendClientCapabilitiesPDU() {
	slog.Debug("Send Client Clipboard Capabilities PDU")
	var cs CliprdrGeneralCapabilitySet
	cs.CapabilitySetLength = 12
	cs.CapabilitySetType = CB_CAPSTYPE_GENERAL
	cs.Version = CB_CAPS_VERSION_2
	cs.GeneralFlags = CB_USE_LONG_FORMAT_NAMES |
		CB_STREAM_FILECLIP_ENABLED |
		CB_FILECLIP_NO_FILE_PATHS
	body := &bytes.Buffer{}
	core.WriteUInt16LE(1, body) // cCapabilitiesSets
	core.WriteUInt16LE(0, body) // pad
	struc.Pack(body, cs)
	sendClipPDU(c.w, CB_CLIP_CAPS, 0, body.Bytes())
}

func (c *CliprdrClient) sendTemporaryDirectoryPDU() {
	slog.Debug("Send Temporary Directory PDU")
	body := make([]byte, 260)
	copy(body, core.UnicodeEncode(os.TempDir()))
	sendClipPDU(c.w, CB_TEMP_DIRECTORY, 0, body)
}

func (c *CliprdrClient) sendFormatListPDU() {
	slog.Debug("Send Format List PDU")
	formats := GetFormatList(c.hwnd)
	slog.Debug("NumFormats:", len(formats))
	slog.Debug("Formats:", formats)

	body := &bytes.Buffer{}
	for _, v := range formats {
		core.WriteUInt32LE(v.FormatId, body)
		if v.FormatName == "" {
			core.WriteUInt16LE(0, body)
		} else {
			n := core.UnicodeEncode(v.FormatName)
			core.WriteBytes(n, body)
			body.Write([]byte{0, 0})
		}
	}
	sendClipPDU(c.w, CB_FORMAT_LIST, 0, body.Bytes())
}

func (c *CliprdrClient) readForamtList(b []byte) (*CliprdrFormatList, bool) {
	r := bytes.NewReader(b)
	fs := make([]CliprdrFormat, 0, 20)
	var numFormats uint32 = 0
	hasFile := false
	c.formatIdMap = make(map[uint32]uint32, 0)
	for r.Len() > 0 {
		foramtId, _ := core.ReadUInt32LE(r)
		bs := make([]uint16, 0, 20)
		ln := r.Len()
		for j := 0; j < ln; j++ {
			b, _ := core.ReadUint16LE(r)
			if b == 0 {
				break
			}
			bs = append(bs, b)
		}
		name := string(utf16.Decode(bs))
		if strings.EqualFold(name, CFSTR_FILEDESCRIPTORW) {
			hasFile = true
		}
		slog.Debug(fmt.Sprintf("Format:%d Name:<%s>", foramtId, name))
		if name != "" {
			localId := RegisterClipboardFormat(name)
			slog.Debug("local:", localId, "remote:", foramtId)
			c.formatIdMap[localId] = foramtId
		} else {
			c.formatIdMap[foramtId] = foramtId
		}

		numFormats++
		fs = append(fs, CliprdrFormat{foramtId, name})
	}

	return &CliprdrFormatList{numFormats, fs}, hasFile
}

func (c *CliprdrClient) sendFormatListResponse(flags uint16) {
	slog.Debug("Send Format List Response")
	sendClipPDU(c.w, CB_FORMAT_LIST_RESPONSE, flags, nil)
}

func (c *CliprdrClient) sendFormatDataRequest(id uint32) {
	slog.Debug("Send Format Data Request")
	body := &bytes.Buffer{}
	core.WriteUInt32LE(id, body)
	sendClipPDU(c.w, CB_FORMAT_DATA_REQUEST, 0, body.Bytes())
}

func (c *CliprdrClient) sendFormatDataResponse(b []byte) {
	slog.Debug("Send Format Data Response")
	sendClipPDU(c.w, CB_FORMAT_DATA_RESPONSE, CB_RESPONSE_OK, b)
}

func (c *CliprdrClient) sendFormatContentsRequest(r CliprdrFileContentsRequest) {
	slog.Debug("Send Format Contents Request")
	slog.Debug(fmt.Sprintf("Format Contents Request:%+v", r))
	body := &bytes.Buffer{}
	core.WriteUInt32LE(r.StreamId, body)
	core.WriteUInt32LE(uint32(r.Lindex), body)
	core.WriteUInt32LE(r.DwFlags, body)
	core.WriteUInt32LE(r.NPositionLow, body)
	core.WriteUInt32LE(r.NPositionHigh, body)
	core.WriteUInt32LE(r.CbRequested, body)
	core.WriteUInt32LE(r.ClipDataId, body)
	sendClipPDU(c.w, CB_FILECONTENTS_REQUEST, 0, body.Bytes())
}

func (c *CliprdrClient) sendFormatContentsResponse(streamId uint32, b []byte) {
	slog.Debug("Send Format Contents Response")
	body := &bytes.Buffer{}
	core.WriteUInt32LE(streamId, body)
	core.WriteBytes(b, body)
	sendClipPDU(c.w, CB_FILECONTENTS_RESPONSE, CB_RESPONSE_OK, body.Bytes())
}

func (c *CliprdrClient) sendLockClipData() {
	slog.Debug("Send Lock Clip Data")
	body := &bytes.Buffer{}
	core.WriteUInt32LE(0, body) // ClipDataId
	sendClipPDU(c.w, CB_LOCK_CLIPDATA, 0, body.Bytes())
}

func (c *CliprdrClient) sendUnlockClipData() {
	slog.Debug("Send Unlock Clip Data")
	body := &bytes.Buffer{}
	core.WriteUInt32LE(0, body) // ClipDataId
	sendClipPDU(c.w, CB_UNLOCK_CLIPDATA, 0, body.Bytes())
}
