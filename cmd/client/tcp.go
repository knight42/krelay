package main

import (
	"io"
	"net"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/xio"
	"github.com/knight42/krelay/pkg/xnet"
)

func handleTCPConn(clientConn net.Conn, serverConn httpstream.Connection, dstAddr xnet.Addr, dstPort uint16) {
	defer clientConn.Close()

	requestID := uuid.New()
	kvs := []interface{}{constants.LogFieldRequestID, requestID.String()}
	defer klog.V(4).InfoS("handleTCPConn exit", kvs...)
	klog.InfoS("Handling tcp connection",
		constants.LogFieldRequestID, requestID.String(),
		constants.LogFieldDestAddr, xnet.JoinHostPort(dstAddr.String(), dstPort),
		constants.LogFieldLocalAddr, clientConn.LocalAddr().String(),
		"clientAddr", clientConn.RemoteAddr().String(),
	)

	dataStream, errorChan, err := createStream(serverConn, requestID.String())
	if err != nil {
		klog.ErrorS(err, "Fail to create stream", kvs...)
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
		klog.ErrorS(err, "Fail to write header", kvs...)
		return
	}

	localError := make(chan struct{})
	remoteDone := make(chan struct{})

	go func() {
		// Copy from the remote side to the local port.
		if _, err := io.Copy(clientConn, dataStream); err != nil && !xnet.IsClosedConnectionError(err) {
			klog.ErrorS(err, "Fail to copy from remote stream to local connection", kvs...)
		}

		// inform the select below that the remote copy is done
		close(remoteDone)
	}()

	go func() {
		// inform server we're not sending any more data after copy unblocks
		defer dataStream.Close()

		// Copy from the local port to the remote side.
		if _, err := io.Copy(dataStream, clientConn); err != nil && !xnet.IsClosedConnectionError(err) {
			klog.ErrorS(err, "Fail to copy from local connection to remote stream", kvs...)
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
		klog.ErrorS(err, "Unexpected error from stream", kvs...)
	}
}
