package x224

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lunixbochs/struc"
	"github.com/nakagami/grdp/core"
	"github.com/nakagami/grdp/emission"
	"github.com/nakagami/grdp/protocol/tpkt"
)

// take idea from https://github.com/Madnikulin50/gordp

/**
 * Message type present in X224 packet header
 */
type MessageType byte

const (
	TPDU_CONNECTION_REQUEST MessageType = 0xE0
	TPDU_CONNECTION_CONFIRM             = 0xD0
	TPDU_DISCONNECT_REQUEST             = 0x80
	TPDU_DATA                           = 0xF0
	TPDU_ERROR                          = 0x70
)

/**
 * Type of negotiation present in negotiation packet
 */
type NegotiationType byte

const (
	TYPE_RDP_NEG_REQ     NegotiationType = 0x01
	TYPE_RDP_NEG_RSP                     = 0x02
	TYPE_RDP_NEG_FAILURE                 = 0x03
)

/**
 * Protocols available for x224 layer
 */

const (
	PROTOCOL_RDP       uint32 = 0x00000000
	PROTOCOL_SSL              = 0x00000001
	PROTOCOL_HYBRID           = 0x00000002
	PROTOCOL_HYBRID_EX        = 0x00000008
)

/**
 * Use to negotiate security layer of RDP stack
 * In node-rdpjs only ssl is available
 * @param opt {object} component type options
 * @see request -> http://msdn.microsoft.com/en-us/library/cc240500.aspx
 * @see response -> http://msdn.microsoft.com/en-us/library/cc240506.aspx
 * @see failure ->http://msdn.microsoft.com/en-us/library/cc240507.aspx
 */
type Negotiation struct {
	Type   NegotiationType `struc:"byte"`
	Flag   uint8           `struc:"uint8"`
	Length uint16          `struc:"little"`
	Result uint32          `struc:"little"`
}

func NewNegotiation() *Negotiation {
	return &Negotiation{0, 0, 0x0008 /*constant*/, PROTOCOL_RDP}
}

type failureCode int

const (
	//The server requires that the client support Enhanced RDP Security (section 5.4) with either TLS 1.0, 1.1 or 1.2 (section 5.4.5.1) or CredSSP (section 5.4.5.2). If only CredSSP was requested then the server only supports TLS.
	SSL_REQUIRED_BY_SERVER = 0x00000001

	//The server is configured to only use Standard RDP Security mechanisms (section 5.3) and does not support any External Security Protocols (section 5.4.5).
	SSL_NOT_ALLOWED_BY_SERVER = 0x00000002

	//The server does not possess a valid authentication certificate and cannot initialize the External Security Protocol Provider (section 5.4.5).
	SSL_CERT_NOT_ON_SERVER = 0x00000003

	//The list of requested security protocols is not consistent with the current security protocol in effect. This error is only possible when the Direct Approach (sections 5.4.2.2 and 1.3.1.2) is used and an External Security Protocol (section 5.4.5) is already being used.
	INCONSISTENT_FLAGS = 0x00000004

	//The server requires that the client support Enhanced RDP Security (section 5.4) with CredSSP (section 5.4.5.2).
	HYBRID_REQUIRED_BY_SERVER = 0x00000005

	//The server requires that the client support Enhanced RDP Security (section 5.4) with TLS 1.0, 1.1 or 1.2 (section 5.4.5.1) and certificate-based client authentication.<4>
	SSL_WITH_USER_AUTH_REQUIRED_BY_SERVER = 0x00000006
)

/**
 * X224 client connection request
 * @param opt {object} component type options
 * @see	http://msdn.microsoft.com/en-us/library/cc240470.aspx
 */
type ClientConnectionRequestPDU struct {
	Len               uint8
	Code              MessageType
	Padding1          uint16
	Padding2          uint16
	Padding3          uint8
	Cookie            []byte
	requestedProtocol uint32
	ProtocolNeg       *Negotiation
}

func NewClientConnectionRequestPDU(cookie []byte, requestedProtocol uint32) *ClientConnectionRequestPDU {
	x := ClientConnectionRequestPDU{0, TPDU_CONNECTION_REQUEST, 0, 0, 0,
		cookie, requestedProtocol, NewNegotiation()}

	x.Len = 6
	if len(cookie) > 0 {
		x.Len += uint8(len(cookie) + 2)
	}
	if x.requestedProtocol > PROTOCOL_RDP {
		x.Len += 8
	}

	return &x
}

func (x *ClientConnectionRequestPDU) Serialize() []byte {
	buff := &bytes.Buffer{}
	core.WriteUInt8(x.Len, buff)
	core.WriteUInt8(uint8(x.Code), buff)
	core.WriteUInt16BE(x.Padding1, buff)
	core.WriteUInt16BE(x.Padding2, buff)
	core.WriteUInt8(x.Padding3, buff)

	if len(x.Cookie) > 0 {
		buff.Write(x.Cookie)
		core.WriteUInt8(0x0D, buff)
		core.WriteUInt8(0x0A, buff)
	}

	if x.requestedProtocol > PROTOCOL_RDP {
		struc.Pack(buff, x.ProtocolNeg)
	}

	return buff.Bytes()
}

/**
 * X224 Server connection confirm
 * @param opt {object} component type options
 * @see	http://msdn.microsoft.com/en-us/library/cc240506.aspx
 */
type ServerConnectionConfirm struct {
	Len         uint8
	Code        MessageType
	Padding1    uint16
	Padding2    uint16
	Padding3    uint8
	ProtocolNeg *Negotiation
}

/**
 * Header of each data message from x224 layer
 * @returns {type.Component}
 */
type DataHeader struct {
	Header      uint8       `struc:"little"`
	MessageType MessageType `struc:"uint8"`
	Separator   uint8       `struc:"little"`
}

func NewDataHeader() *DataHeader {
	return &DataHeader{2, TPDU_DATA /* constant */, 0x80 /*constant*/}
}

/**
 * Common X224 Automata
 * @param presentation {Layer} presentation layer
 */
type X224 struct {
	emission.Emitter
	transport         core.Transport
	requestedProtocol uint32
	selectedProtocol  uint32
	dataHeader        *DataHeader
}

func New(t core.Transport) *X224 {
	x := &X224{
		*emission.NewEmitter(),
		t,
		PROTOCOL_RDP | PROTOCOL_SSL | PROTOCOL_HYBRID,
		PROTOCOL_SSL,
		NewDataHeader(),
	}

	t.On("close", func() {
		x.Emit("close")
	}).On("error", func(err error) {
		x.Emit("error", err)
	})

	return x
}

func (x *X224) Read(b []byte) (n int, err error) {
	return x.transport.Read(b)
}

func (x *X224) Write(b []byte) (n int, err error) {
	buff := &bytes.Buffer{}
	err = struc.Pack(buff, x.dataHeader)
	if err != nil {
		return 0, err
	}
	buff.Write(b)

	return x.transport.Write(buff.Bytes())
}

func (x *X224) Close() error {
	return x.transport.Close()
}

func (x *X224) SetRequestedProtocol(p uint32) {
	x.requestedProtocol = p
}

func (x *X224) Connect() error {
	if x.transport == nil {
		return errors.New("no transport")
	}
	cookie := "Cookie: mstshash=test"
	message := NewClientConnectionRequestPDU([]byte(cookie), x.requestedProtocol)
	message.ProtocolNeg.Type = TYPE_RDP_NEG_REQ
	message.ProtocolNeg.Result = uint32(x.requestedProtocol)

	slog.Debug("x224 Connect", "message", hex.EncodeToString(message.Serialize()))
	_, err := x.transport.Write(message.Serialize())
	x.transport.Once("data", x.recvConnectionConfirm)
	return err
}

func (x *X224) recvConnectionConfirm(s []byte) {
	slog.Debug("x224 recvConnectionConfirm", "s", hex.EncodeToString(s))
	r := bytes.NewReader(s)
	ln, _ := core.ReadUInt8(r)
	if ln > 6 {
		message := &ServerConnectionConfirm{}
		if err := struc.Unpack(bytes.NewReader(s), message); err != nil {
			slog.Error("ReadServerConnectionConfirm", "err", err)
			return
		}
		slog.Debug("recvConnectionConfirm", "message", *message.ProtocolNeg)
		if message.ProtocolNeg.Type == TYPE_RDP_NEG_FAILURE {
			slog.Error(fmt.Sprintf("NODE_RDP_PROTOCOL_X224_NEG_FAILURE with code: %d,see https://msdn.microsoft.com/en-us/library/cc240507.aspx",
				message.ProtocolNeg.Result))
			//only use Standard RDP Security mechanisms
			if message.ProtocolNeg.Result == 2 {
				slog.Info("Only use Standard RDP Security mechanisms, Reconnect with Standard RDP")
			}
			x.Close()
			return
		}

		if message.ProtocolNeg.Type == TYPE_RDP_NEG_RSP {
			slog.Info("TYPE_RDP_NEG_RSP")
			x.selectedProtocol = message.ProtocolNeg.Result
		}
	} else {
		x.selectedProtocol = PROTOCOL_RDP
	}

	if x.selectedProtocol == PROTOCOL_HYBRID_EX {
		slog.Error("NODE_RDP_PROTOCOL_HYBRID_EX_NOT_SUPPORTED")
		return
	}

	x.transport.On("data", x.recvData)

	if x.selectedProtocol == PROTOCOL_RDP {
		slog.Info("*** RDP security selected ***")
		x.Emit("connect", x.selectedProtocol)
		return
	}

	if x.selectedProtocol == PROTOCOL_SSL {
		slog.Info("*** SSL security selected ***")
		err := x.transport.(*tpkt.TPKT).StartTLS()
		if err != nil {
			slog.Error("start tls failed:", "err", err)
			return
		}
		x.Emit("connect", x.selectedProtocol)
		return
	}

	if x.selectedProtocol == PROTOCOL_HYBRID {
		slog.Info("*** NLA Security selected ***")
		err := x.transport.(*tpkt.TPKT).StartNLA()
		if err != nil {
			slog.Error("start NLA failed:", "err", err)
			return
		}
		x.Emit("connect", x.selectedProtocol)
		return
	}
}

func (x *X224) recvData(s []byte) {
	// x224 header takes 3 bytes
	x.Emit("data", s[3:])
}
