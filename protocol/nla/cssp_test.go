package nla_test

import (
	"encoding/hex"
	"testing"

	"github.com/nakagami/grdp/protocol/nla"
)

func TestEncodeDERTRequest(t *testing.T) {
	ntlm := nla.NewNTLMv2("", "", "")
	result := nla.EncodeDERTRequest([]nla.Message{ntlm.GetNegotiateMessage()}, []byte{}, []byte{})
	if hex.EncodeToString(result) != "3037a003020102a130302e302ca02a04284e544c4d535350000100000035820860000000000000000000000000000000000000000000000000" {
		t.Error("not equal")
	}
}
