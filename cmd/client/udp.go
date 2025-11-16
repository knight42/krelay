package main

import (
	"log/slog"
	"net"

	"k8s.io/apimachinery/pkg/util/httpstream"

	"github.com/knight42/krelay/pkg/constants"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xio"
	"github.com/knight42/krelay/pkg/xnet"
)

func handleUDPConn(clientConn net.PacketConn, cliAddr net.Addr, dataCh chan []byte, finish chan<- string, serverConn httpstream.Connection, dstAddrPort xnet.AddrPort) {
	requestID := xnet.NewRequestID()
	l := slog.With(slog.String(constants.LogFieldRequestID, requestID))
	defer l.Debug("handleUDPConn exit")
	defer func() {
		finish <- cliAddr.String()
	}()
	l.Info("Handling udp connection",
		slog.String(constants.LogFieldDestAddr, dstAddrPort.String()),
		slog.String(constants.LogFieldLocalAddr, clientConn.LocalAddr().String()),
		slog.String("clientAddr", cliAddr.String()),
	)

	dataStream, errorChan, err := createStream(serverConn, requestID)
	if err != nil {
		l.Error("Fail to create stream", slogutil.Error(err))
		return
	}

	hdr := xnet.Header{
		RequestID: requestID,
		Protocol:  xnet.ProtocolUDP,
		Port:      dstAddrPort.Port(),
		Addr:      dstAddrPort.Addr(),
	}
	_, err = xio.WriteFull(dataStream, hdr.Marshal())
	if err != nil {
		l.Error("Fail to write header", slogutil.Error(err))
		return
	}

	var ack xnet.Acknowledgement
	err = ack.FromReader(dataStream)
	if err != nil {
		l.Error("Fail to receive ack", slogutil.Error(err))
		return
	}
	if ack.Code != xnet.AckCodeOK {
		l.Error("Fail to connect", slogutil.Error(ack.Code))
		return
	}

	upClosed := make(chan struct{})
	go func() {
		var (
			data []byte
			ok   bool
		)
		for {
			select {
			case data, ok = <-dataCh:
				if !ok {
					return
				}
			case <-upClosed:
				return
			}
			_, err = xio.WriteFull(dataStream, data)
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer l.Debug("Server close connection")
		defer close(upClosed)

		buf := make([]byte, constants.UDPBufferSize)
		for {
			n, err := xnet.ReadUDPFromStream(dataStream, buf, 0)
			if err != nil {
				return
			}

			_, err = clientConn.WriteTo(buf[:n], cliAddr)
			if err != nil {
				return
			}
		}
	}()

	// always expect something on errorChan (it may be nil)
	err = <-errorChan
	if err != nil {
		l.Error("Unexpected error from stream", slogutil.Error(err))
	}
}
