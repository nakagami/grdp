package core

import (
	"crypto/rsa"
	"crypto/tls"
	"encoding/asn1"
	"errors"
	"math/big"
	"net"
	"time"
)

type SocketLayer struct {
	conn    net.Conn
	tlsConn *tls.Conn
}

func NewSocketLayer(conn net.Conn) *SocketLayer {
	l := &SocketLayer{
		conn:    conn,
		tlsConn: nil,
	}
	return l
}

func (s *SocketLayer) SetDeadline(t time.Time) error {
	return s.conn.SetDeadline(t)
}

func (s *SocketLayer) Read(b []byte) (n int, err error) {
	if s.tlsConn != nil {
		return s.tlsConn.Read(b)
	}
	return s.conn.Read(b)
}

func (s *SocketLayer) Write(b []byte) (n int, err error) {
	if s.tlsConn != nil {
		return s.tlsConn.Write(b)
	}
	return s.conn.Write(b)
}

func (s *SocketLayer) Close() error {
	if s.tlsConn != nil {
		s.tlsConn.Close() // best-effort; always close the underlying TCP socket
	}
	return s.conn.Close()
}

func (s *SocketLayer) StartTLS() error {
	config := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		//		MaxVersion:               tls.VersionTLS13,
	}
	s.tlsConn = tls.Client(s.conn, config)
	return s.tlsConn.Handshake()
}

type PublicKey struct {
	N *big.Int `asn1:"explicit,tag:0"` // modulus
	E int      `asn1:"explicit,tag:1"` // public exponent
}

func (s *SocketLayer) TlsPubKey() ([]byte, error) {
	if s.tlsConn == nil {
		return nil, errors.New("TLS conn does not exist")
	}
	pub := s.tlsConn.ConnectionState().PeerCertificates[0].PublicKey.(*rsa.PublicKey)
	return asn1.Marshal(*pub)
}
