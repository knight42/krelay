package xnet

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/google/uuid"
)

const totalLenOfOtherFields = 23 // 1(version) + 2(total length) + 16(uuid) + 1(protocol) + 2(port) + 1(addr type)

type Header struct {
	Version   byte
	RequestID uuid.UUID
	Protocol  byte
	Port      uint16
	Addr      Addr
}

func (h *Header) Marshal() []byte {
	addrBytes := h.Addr.Marshal()
	totalLen := totalLenOfOtherFields + len(addrBytes)
	buf := make([]byte, totalLen)
	buf[0] = h.Version
	binary.BigEndian.PutUint16(buf[1:3], uint16(totalLen))
	copy(buf[3:19], h.RequestID[:])
	buf[19] = h.Protocol
	binary.BigEndian.PutUint16(buf[20:22], h.Port)
	buf[22] = h.Addr.Type
	copy(buf[23:], addrBytes)
	return buf
}

func (h *Header) FromReader(r io.Reader) error {
	var lengthBuf [3]byte
	_, err := io.ReadFull(r, lengthBuf[:])
	if err != nil {
		return fmt.Errorf("read total length: %w", err)
	}
	h.Version = lengthBuf[0]
	// TODO: handle different versions
	totalLen := binary.BigEndian.Uint16(lengthBuf[1:])
	if totalLen < totalLenOfOtherFields {
		return fmt.Errorf("body too short: %d", totalLen)
	}

	bodyBuf := make([]byte, totalLen-3)
	_, err = io.ReadFull(r, bodyBuf)
	if err != nil {
		return fmt.Errorf("read full body: %w", err)
	}

	reqIDBytes := bodyBuf[:16]
	proto := bodyBuf[16]
	port := binary.BigEndian.Uint16(bodyBuf[17:19])
	h.RequestID, _ = uuid.FromBytes(reqIDBytes)
	h.Protocol = proto
	h.Port = port
	h.Addr = AddrFromBytes(bodyBuf[19], bodyBuf[20:])
	return nil
}
