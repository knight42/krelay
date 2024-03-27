package main

import (
	"io"
	"log/slog"
	"net"

	"k8s.io/apimachinery/pkg/util/httpstream"

	"github.com/knight42/krelay/pkg/constants"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xio"
	"github.com/knight42/krelay/pkg/xnet"
)

func handleTCPConn(clientConn net.Conn, serverConn httpstream.Connection, dstAddr xnet.Addr, dstPort uint16) {
	defer clientConn.Close()

	requestID := xnet.NewRequestID()
	l := slog.With(slog.String(constants.LogFieldRequestID, requestID))
	defer l.Debug("handleTCPConn exit")
	l.Info("Handling tcp connection",
		slog.String(constants.LogFieldDestAddr, xnet.JoinHostPort(dstAddr.String(), dstPort)),
		slog.String(constants.LogFieldLocalAddr, clientConn.LocalAddr().String()),
		slog.String("clientAddr", clientConn.RemoteAddr().String()),
	)

	dataStream, errorChan, err := createStream(serverConn, requestID)
	if err != nil {
		l.Error("Fail to create stream", slogutil.Error(err))
		return
	}

	hdr := xnet.Header{
		RequestID: requestID,
		Protocol:  xnet.ProtocolTCP,
		Port:      dstPort,
		Addr:      dstAddr,
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

	localError := make(chan struct{})
	remoteDone := make(chan struct{})

	go func() {
		// Copy from the remote side to the local port.
		if _, err := io.Copy(clientConn, dataStream); err != nil && !xnet.IsClosedConnectionError(err) {
			l.Error("Fail to copy from remote stream to local connection", slogutil.Error(err))
		}

		// inform the select below that the remote copy is done
		close(remoteDone)
	}()

	go func() {
		// inform server we're not sending any more data after copy unblocks
		defer dataStream.Close()

		// Copy from the local port to the remote side.
		if _, err := io.Copy(dataStream, clientConn); err != nil && !xnet.IsClosedConnectionError(err) {
			l.Error("Fail to copy from local connection to remote stream", slogutil.Error(err))
			// break out of the select below without waiting for the other copy to finish
			close(localError)
		}
	}()

	// wait for either a local->remote error or for copying from remote->local to finish
	select {
	case <-remoteDone:
	case <-localError:
	}

	// always expect something on errorChan (it may be nil)
	err = <-errorChan
	if err != nil {
		l.Error("Unexpected error from stream", slogutil.Error(err))
	}
}
