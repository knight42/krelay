// Package protocol implements the tiny handshake exchanged on every tunnel
// connection between the krelay client and krelay-server.
//
// The client opens a TCP connection to the server's tailcat address and sends
// a dial request naming the real destination; the server dials it and reports
// the outcome. After a successful handshake the connection carries raw
// application bytes in both directions.
//
//	client -> server: magic "KRL3" | uint16(len) | target "host:port"
//	server -> client: status byte (0 = connected) | uint16(len) | error message
//
// An empty target denotes a heartbeat connection: the server does not dial
// anything and simply holds the connection open, keeping itself alive.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

var magic = [4]byte{'K', 'R', 'L', '3'}

// WriteDialRequest sends a dial request for target ("host:port", or empty for
// a heartbeat connection) to w.
func WriteDialRequest(w io.Writer, target string) error {
	if len(target) > math.MaxUint16 {
		return fmt.Errorf("target too long: %d bytes", len(target))
	}
	buf := make([]byte, 0, len(magic)+2+len(target))
	buf = append(buf, magic[:]...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(target)))
	buf = append(buf, target...)
	_, err := w.Write(buf)
	return err
}

// ReadDialRequest reads a dial request from r and returns the target.
func ReadDialRequest(r io.Reader) (string, error) {
	var hdr [6]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	if [4]byte(hdr[:4]) != magic {
		return "", fmt.Errorf("bad magic: %q", hdr[:4])
	}
	target := make([]byte, binary.BigEndian.Uint16(hdr[4:]))
	if _, err := io.ReadFull(r, target); err != nil {
		return "", err
	}
	return string(target), nil
}

// WriteDialResponse reports the result of dialing the requested target.
// A nil dialErr means the destination is connected.
func WriteDialResponse(w io.Writer, dialErr error) error {
	if dialErr == nil {
		_, err := w.Write([]byte{0})
		return err
	}
	msg := dialErr.Error()
	if len(msg) > math.MaxUint16 {
		msg = msg[:math.MaxUint16]
	}
	buf := make([]byte, 0, 3+len(msg))
	buf = append(buf, 1)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(msg)))
	buf = append(buf, msg...)
	_, err := w.Write(buf)
	return err
}

// ReadDialResponse reads the server's dial response. It returns nil if the
// destination is connected, or an error carrying the server-side message.
func ReadDialResponse(r io.Reader) error {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return fmt.Errorf("read dial status: %w", err)
	}
	if status[0] == 0 {
		return nil
	}
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return fmt.Errorf("read dial error: %w", err)
	}
	msg := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(r, msg); err != nil {
		return fmt.Errorf("read dial error: %w", err)
	}
	return errors.New(string(msg))
}
