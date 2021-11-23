package xnet

import (
	"io"
	"net"

	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
)

var tcpPool = newBufferPool(constants.TCPBufferSize)

// This does the actual data transfer.
// The broker only closes the Read side.
func tcpBroker(dst, src net.Conn, srcClosed chan struct{}) {
	defer src.Close()
	buf := tcpPool.Get().([]byte)
	defer tcpPool.Put(buf)

	// We can handle errors in a finer-grained manner by inlining io.Copy (it's
	// simple, and we drop the ReaderFrom or WriterTo checks for
	// net.Conn->net.Conn transfers, which aren't needed). This would also let
	// us adjust buffersize.
	_, _ = io.CopyBuffer(dst, src, buf)

	close(srcClosed)
}

// ProxyTCP is excerpt from https://stackoverflow.com/a/27445109/4725840
func ProxyTCP(reqID string, downConn, upConn *net.TCPConn) {
	defer klog.V(4).InfoS("ProxyTCP exit", constants.LogFieldRequestID, reqID)

	// channels to wait on the close event for each connection
	upClosed := make(chan struct{})
	downClosed := make(chan struct{})

	go tcpBroker(upConn, downConn, downClosed)
	go tcpBroker(downConn, upConn, upClosed)

	// wait for one half of the proxy to exit, then trigger a shutdown of the
	// other half by calling CloseRead(). This will break the read loop in the
	// broker and allow us to fully close the connection cleanly without a
	// "use of closed network connection" error.
	var waitFor chan struct{}
	select {
	case <-downClosed:
		klog.V(4).InfoS("Client close connection", constants.LogFieldRequestID, reqID)
		// the client closed first and any more packets from the server aren't
		// useful, so we can optionally SetLinger(0) here to recycle the port
		// faster.
		_ = upConn.SetLinger(0)
		_ = upConn.CloseRead()
		waitFor = upClosed
	case <-upClosed:
		klog.V(4).InfoS("Server close connection", constants.LogFieldRequestID, reqID)
		_ = downConn.CloseRead()
		waitFor = downClosed
	}

	// Wait for the other connection to close.
	// This "waitFor" pattern isn't required, but gives us a way to track the
	// connection and ensure all copies terminate correctly; we can trigger
	// stats on entry and deferred exit of this function.
	<-waitFor
}
