package main

import (
	"net"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/xio"
	"github.com/knight42/krelay/pkg/xnet"
)

func handleUDPConn(clientConn net.PacketConn, cliAddr net.Addr, dataCh chan []byte, finish chan<- string, serverConn httpstream.Connection, dstAddr xnet.Addr, dstPort uint16) {
	requestID := uuid.New()
	kvs := []any{constants.LogFieldDestAddr, requestID.String()}
	defer klog.V(4).InfoS("handleUDPConn exit", kvs...)
	defer func() {
		finish <- cliAddr.String()
	}()
	klog.InfoS("Handling udp connection",
		constants.LogFieldRequestID, requestID.String(),
		constants.LogFieldDestAddr, xnet.JoinHostPort(dstAddr.String(), dstPort),
		constants.LogFieldLocalAddr, clientConn.LocalAddr().String(),
		"clientAddr", cliAddr.String(),
	)

	dataStream, errorChan, err := createStream(serverConn, requestID.String())
	if err != nil {
		klog.ErrorS(err, "Fail to create stream", kvs...)
		return
	}

	hdr := xnet.Header{
		RequestID: requestID,
		Protocol:  xnet.ProtocolUDP,
		Port:      dstPort,
		Addr:      dstAddr,
	}
	_, err = xio.WriteFull(dataStream, hdr.Marshal())
	if err != nil {
		klog.ErrorS(err, "Fail to write header", kvs...)
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
		defer klog.V(4).InfoS("Server close connection", kvs...)
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
		klog.ErrorS(err, "Unexpected error from stream", kvs...)
	}
}
