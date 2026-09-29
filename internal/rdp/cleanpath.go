package rdp

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// RDCleanPath is the handshake IronRDP's web client runs with its WebSocket proxy (RESEARCH §3.10 Path A). The first
// WebSocket message is a DER encoded RDCleanPathPdu request carrying the X.224 Connection Request, the proxy
// authorization token and optionally a pre-connection blob. The proxy connects to the RDP server, forwards the
// X.224 request, reads the Connection Confirm, upgrades to TLS and answers with a response PDU holding the confirm and
// the server's certificate chain (the browser needs it for CredSSP public-key binding). From then on the WebSocket
// carries the TLS plaintext of the RDP connection.
//
//	RDCleanPathPdu ::= SEQUENCE {           -- every field EXPLICIT, all optional except version
//	    version             [0] INTEGER,    -- 3390 (3389 + 1)
//	    error               [1] RDCleanPathErr,
//	    destination         [2] UTF8String,
//	    proxy_auth          [3] UTF8String,
//	    server_auth         [4] UTF8String,
//	    preconnection_blob  [5] UTF8String,
//	    x224_connection_pdu [6] OCTET STRING,
//	    server_cert_chain   [7] SEQUENCE OF OCTET STRING,
//	    server_addr         [9] UTF8String }
//	RDCleanPathErr ::= SEQUENCE {
//	    error_code       [0] INTEGER,       -- 1 general, 2 negotiation
//	    http_status_code [1] INTEGER OPTIONAL,
//	    wsa_last_error   [2] INTEGER OPTIONAL,
//	    tls_alert_code   [3] INTEGER OPTIONAL }

const (
	cleanPathVersion = 3390

	cleanPathGeneralError     = 1
	cleanPathNegotiationError = 2

	// maxCleanPathPDU bounds the request PDU (X.224 request + token + PCB are small).
	maxCleanPathPDU = 64 << 10
)

type cleanPathErr struct {
	ErrorCode      int `asn1:"explicit,tag:0"`
	HTTPStatusCode int `asn1:"optional,explicit,tag:1"`
	WSALastError   int `asn1:"optional,explicit,tag:2"`
	TLSAlertCode   int `asn1:"optional,explicit,tag:3"`
}

type cleanPathPDU struct {
	Version           int64        `asn1:"explicit,tag:0"`
	Error             cleanPathErr `asn1:"optional,explicit,tag:1"`
	Destination       string       `asn1:"optional,explicit,tag:2,utf8"`
	ProxyAuth         string       `asn1:"optional,explicit,tag:3,utf8"`
	ServerAuth        string       `asn1:"optional,explicit,tag:4,utf8"`
	PreconnectionBlob string       `asn1:"optional,explicit,tag:5,utf8"`
	X224ConnectionPDU []byte       `asn1:"optional,explicit,tag:6"`
	ServerCertChain   [][]byte     `asn1:"optional,explicit,tag:7"`
	ServerAddr        string       `asn1:"optional,explicit,tag:9,utf8"`
}

// decodeCleanPath parses a DER RDCleanPathPdu.
func decodeCleanPath(b []byte) (*cleanPathPDU, error) {
	var p cleanPathPDU
	rest, err := asn1.Unmarshal(b, &p)
	if err != nil {
		return nil, fmt.Errorf("invalid RDCleanPath PDU: %w", err)
	}
	if len(rest) > 0 {
		return nil, errors.New("invalid RDCleanPath PDU: trailing data")
	}
	return &p, nil
}

// encode renders the PDU as DER.
func (p *cleanPathPDU) encode() ([]byte, error) { return asn1.Marshal(*p) }

// cleanPathResponse is the success response: X.224 Connection Confirm, the server's certificate chain (DER, leaf
// first) and the address the proxy connected to.
func cleanPathResponse(x224 []byte, chain [][]byte, serverAddr string) ([]byte, error) {
	return (&cleanPathPDU{
		Version:           cleanPathVersion,
		X224ConnectionPDU: x224,
		ServerCertChain:   chain,
		ServerAddr:        serverAddr,
	}).encode()
}

// cleanPathFailure describes an error reported to the client in the RDCleanPath error field.
type cleanPathFailure struct {
	Code       int // cleanPathGeneralError | cleanPathNegotiationError
	HTTPStatus int
	WSAError   int
	TLSAlert   int
	// X224 is the server's Connection Confirm carrying an RDP_NEG_FAILURE (negotiation errors).
	X224 []byte
}

func (f cleanPathFailure) encode() ([]byte, error) {
	code := f.Code
	if code == 0 {
		code = cleanPathGeneralError
	}
	p := &cleanPathPDU{
		Version: cleanPathVersion,
		Error: cleanPathErr{
			ErrorCode:      code,
			HTTPStatusCode: f.HTTPStatus,
			WSALastError:   f.WSAError,
			TLSAlertCode:   f.TLSAlert,
		},
	}
	if code == cleanPathNegotiationError {
		p.X224ConnectionPDU = f.X224
	}
	return p.encode()
}

// derLength returns the total length of the DER element starting at b, or 0 when more bytes are needed. It fails
// for anything that is not a SEQUENCE or uses an unsupported length form.
func derLength(b []byte) (int, error) {
	if len(b) < 2 {
		return 0, nil
	}
	if b[0] != 0x30 {
		return 0, errors.New("RDCleanPath PDU must be a DER SEQUENCE")
	}
	l := int(b[1])
	if l < 0x80 {
		return 2 + l, nil
	}
	n := l & 0x7f
	if n == 0 || n > 4 {
		return 0, errors.New("unsupported DER length encoding")
	}
	if len(b) < 2+n {
		return 0, nil
	}
	size := 0
	for _, c := range b[2 : 2+n] {
		size = size<<8 | int(c)
	}
	if size < 0 || size > maxCleanPathPDU {
		return 0, errors.New("RDCleanPath PDU too large")
	}
	return 2 + n + size, nil
}
