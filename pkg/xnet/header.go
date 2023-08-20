package xnet

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
)

const (
	lengthAllMandatoryFields = 12 // 1(version) + 2(total length) + 5(request id) + 1(protocol) + 2(port) + 1(addr type)
	lengthRequestID          = 5
)

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

func NewRequestID() string {
	b := make([]rune, lengthRequestID)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

type Header struct {
	Version   byte
	RequestID string
	Protocol  byte
	Port      uint16
	Addr      Addr
}

func (h *Header) Marshal() []byte {
	addrBytes := h.Addr.Marshal()
	totalLen := lengthAllMandatoryFields + len(addrBytes)
	buf := make([]byte, totalLen)

	cursor := 0
	buf[cursor] = h.Version
	cursor++

	binary.BigEndian.PutUint16(buf[cursor:cursor+2], uint16(totalLen))
	cursor += 2

	copy(buf[cursor:cursor+lengthRequestID], h.RequestID)
	cursor += lengthRequestID

	buf[cursor] = h.Protocol
	cursor++

	binary.BigEndian.PutUint16(buf[cursor:cursor+2], h.Port)
	cursor += 2

	buf[cursor] = h.Addr.typ
	cursor++

	copy(buf[cursor:], addrBytes)
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
	if totalLen < lengthAllMandatoryFields {
		return fmt.Errorf("body too short: %d", totalLen)
	}

	bodyBuf := make([]byte, totalLen-3)
	_, err = io.ReadFull(r, bodyBuf)
	if err != nil {
		return fmt.Errorf("read full body: %w", err)
	}

	cursor := 0
	reqIDBytes := bodyBuf[cursor : cursor+lengthRequestID]
	cursor += lengthRequestID

	proto := bodyBuf[cursor]
	cursor++

	port := binary.BigEndian.Uint16(bodyBuf[cursor : cursor+2])
	cursor += 2

	h.RequestID = string(reqIDBytes)
	h.Protocol = proto
	h.Port = port
	h.Addr = AddrFromBytes(bodyBuf[cursor], bodyBuf[cursor+1:])
	return nil
}
