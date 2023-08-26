package xnet

import (
	"fmt"
	"io"
)

type AckCode uint8

const (
	AckCodeOK = iota + 1
	AckCodeUnknownError
	AckCodeNoSuchHost
	AckCodeResolveTimeout
	AckCodeConnectTimeout
)

func (c AckCode) Error() string {
	switch c {
	case AckCodeUnknownError:
		return "Unknown error"
	case AckCodeNoSuchHost:
		return "No such host"
	case AckCodeResolveTimeout:
		return "Resolve timeout"
	case AckCodeConnectTimeout:
		return "Connect timeout"
	}
	return "Unknown Code"
}

type Acknowledgement struct {
	Code AckCode
}

func (a *Acknowledgement) Marshal() []byte {
	return []byte{byte(a.Code)}
}

func (a *Acknowledgement) FromReader(r io.Reader) error {
	var buf [1]byte
	_, err := r.Read(buf[:])
	if err != nil {
		return fmt.Errorf("read ack: %w", err)
	}
	a.Code = AckCode(buf[0])
	return nil
}
