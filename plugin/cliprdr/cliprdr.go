//go:build windows

package cliprdr

import (
	"bytes"
	"encoding/hex"
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

func (c *CliprdrClient) Send(s []byte) (int, error) {
	slog.Debug("len:", len(s), "data:", hex.EncodeToString(s))
	name, _ := c.GetType()
	return c.w.SendToChannel(name, s)
}
func (c *CliprdrClient) Sender(f core.ChannelSender) {
	c.w = f
}
func (c *CliprdrClient) GetType() (string, uint32) {
	return ChannelName, ChannelOption
}

func (c *CliprdrClient) Process(s []byte) {
	slog.Debug("recv:", hex.EncodeToString(s))
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
	//Client Clipboard Capabilities PDU
	c.sendClientCapabilitiesPDU()

	//Temporary Directory PDU
	//c.sendTemporaryDirectoryPDU()

	//Format List PDU
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
	/*o := OleGetClipboard()
	var format_etc FORMATETC
	var stg_medium STGMEDIUM
	format_etc.CFormat = RegisterClipboardFormat(CFSTR_FILECONTENTS)
	format_etc.Tymed = TYMED_ISTREAM
	format_etc.Aspect = 1
	format_etc.Index = req.Lindex
	o.GetData(&format_etc, &stg_medium)
	s, _ := stg_medium.Stream()*/
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
	var cc CliprdrCapabilitiesPDU
	cc.CCapabilitiesSets = 1
	cc.Pad1 = 0
	cc.CapabilitySets = make([]CliprdrGeneralCapabilitySet, 0, 1)
	cc.CapabilitySets = append(cc.CapabilitySets, cs)
	header := NewCliprdrPDUHeader(CB_CLIP_CAPS, 0, 16)

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteUInt16LE(cc.CCapabilitiesSets, buff)
	core.WriteUInt16LE(cc.Pad1, buff)
	for _, v := range cc.CapabilitySets {
		struc.Pack(buff, v)
	}

	c.Send(buff.Bytes())
}

func (c *CliprdrClient) sendTemporaryDirectoryPDU() {
	slog.Debug("Send Temporary Directory PDU")
	var t CliprdrTempDirectory
	header := &CliprdrPDUHeader{CB_TEMP_DIRECTORY, 0, 260}
	t.SzTempDir = core.UnicodeEncode(os.TempDir())

	buff := &bytes.Buffer{}
	core.WriteBytes(header.serialize(), buff)
	core.WriteBytes(t.SzTempDir, buff)
	c.Send(buff.Bytes())
}
func (c *CliprdrClient) sendFormatListPDU() {
	slog.Debug("Send Format List PDU")
	var f CliprdrFormatList

	f.Formats = GetFormatList(c.hwnd)
	f.NumFormats = uint32(len(f.Formats))

	slog.Debug("NumFormats:", f.NumFormats)
	slog.Debug("Formats:", f.Formats)

	b := &bytes.Buffer{}
	for _, v := range f.Formats {
		core.WriteUInt32LE(v.FormatId, b)
		if v.FormatName == "" {
			core.WriteUInt16LE(0, b)
		} else {
			n := core.UnicodeEncode(v.FormatName)
			core.WriteBytes(n, b)
			b.Write([]byte{0, 0})
		}
	}

	header := NewCliprdrPDUHeader(CB_FORMAT_LIST, 0, uint32(b.Len()))

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteBytes(b.Bytes(), buff)

	c.Send(buff.Bytes())
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
		slog.Debugf("Foramt:%d Name:<%s>", foramtId, name)
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
	header := NewCliprdrPDUHeader(CB_FORMAT_LIST_RESPONSE, flags, 0)
	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	c.Send(buff.Bytes())
}

func (c *CliprdrClient) sendFormatDataRequest(id uint32) {
	slog.Debug("Send Format Data Request")
	var r CliprdrFormatDataRequest
	r.RequestedFormatId = id
	header := NewCliprdrPDUHeader(CB_FORMAT_DATA_REQUEST, 0, 4)

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteUInt32LE(r.RequestedFormatId, buff)

	c.Send(buff.Bytes())
}
func (c *CliprdrClient) sendFormatDataResponse(b []byte) {
	slog.Debug("Send Format Data Response")
	var resp CliprdrFormatDataResponse
	resp.RequestedFormatData = b

	header := NewCliprdrPDUHeader(CB_FORMAT_DATA_RESPONSE, CB_RESPONSE_OK, uint32(len(resp.RequestedFormatData)))

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	buff.Write(resp.RequestedFormatData)

	c.Send(buff.Bytes())
}

func (c *CliprdrClient) sendFormatContentsRequest(r CliprdrFileContentsRequest) uint32 {
	slog.Debug("Send Format Contents Request")
	slog.Debug(fmt.Sprintf("Format Contents Request:%+v", r))
	header := NewCliprdrPDUHeader(CB_FILECONTENTS_REQUEST, 0, 28)

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteUInt32LE(r.StreamId, buff)
	core.WriteUInt32LE(uint32(r.Lindex), buff)
	core.WriteUInt32LE(r.DwFlags, buff)
	core.WriteUInt32LE(r.NPositionLow, buff)
	core.WriteUInt32LE(r.NPositionHigh, buff)
	core.WriteUInt32LE(r.CbRequested, buff)
	core.WriteUInt32LE(r.ClipDataId, buff)

	c.Send(buff.Bytes())

	return uint32(buff.Len())
}
func (c *CliprdrClient) sendFormatContentsResponse(streamId uint32, b []byte) {
	slog.Debug("Send Format Contents Response")
	var r CliprdrFileContentsResponse
	r.StreamId = streamId
	r.RequestedData = b
	r.CbRequested = uint32(len(b))
	header := NewCliprdrPDUHeader(CB_FILECONTENTS_RESPONSE, CB_RESPONSE_OK, uint32(4+r.CbRequested))

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteUInt32LE(r.StreamId, buff)
	core.WriteBytes(r.RequestedData, buff)

	c.Send(buff.Bytes())
}

func (c *CliprdrClient) sendLockClipData() {
	slog.Debug("Send Lock Clip Data")
	var r CliprdrCtrlClipboardData
	header := NewCliprdrPDUHeader(CB_LOCK_CLIPDATA, 0, 4)

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteUInt32LE(r.ClipDataId, buff)

	c.Send(buff.Bytes())
}

func (c *CliprdrClient) sendUnlockClipData() {
	slog.Debug("Send Unlock Clip Data")
	var r CliprdrCtrlClipboardData
	header := NewCliprdrPDUHeader(CB_UNLOCK_CLIPDATA, 0, 4)

	buff := &bytes.Buffer{}
	buff.Write(header.serialize())
	core.WriteUInt32LE(r.ClipDataId, buff)

	c.Send(buff.Bytes())
}
