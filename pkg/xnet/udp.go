package xnet

import (
	"encoding/binary"
	"io"
	"net"
	"time"

	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/alarm"
	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/xio"
)

// UDPConn wraps a *net.UDPConn and overrides the Read and ReadFrom methods to
// automatically prepend the length of the packet.
type UDPConn struct {
	*net.UDPConn
}

func (c *UDPConn) ReadFrom(buf []byte) (n int, addr net.Addr, err error) {
	n, addr, err = c.UDPConn.ReadFrom(buf[2:])
	binary.BigEndian.PutUint16(buf[:2], uint16(n))
	return n + 2, addr, err
}

func (c *UDPConn) Read(buf []byte) (n int, err error) {
	n, err = c.UDPConn.Read(buf[2:])
	binary.BigEndian.PutUint16(buf[:2], uint16(n))
	return n + 2, err
}

func ReadUDPFromStream(r io.Reader, buf []byte, timeout time.Duration) (n int, err error) {
	var lengthBuf [2]byte
	_, err = readFullWithTimeout(r, lengthBuf[:], timeout)
	if err != nil {
		return 0, err
	}
	// assume the capacity of the buffer is big enough
	length := binary.BigEndian.Uint16(lengthBuf[:])
	return readFullWithTimeout(r, buf[:length], timeout)
}

var udpPool = newBufferPool(constants.UDPBufferSize)

func ProxyUDP(reqID string, downConn *net.TCPConn, upConn net.Conn) {
	defer klog.V(4).InfoS("ProxyUDP exit", constants.LogFieldRequestID, reqID)

	downClosed := make(chan struct{})
	upClosed := make(chan struct{})

	const idleTimeout = time.Second * 110

	a := alarm.New(idleTimeout)
	a.Start()

	go func() {
		bufPtr := udpPool.Get().(*[]byte)
		defer udpPool.Put(bufPtr)
		defer close(downClosed)

		buf := *bufPtr
		for {
			n, err := ReadUDPFromStream(downConn, buf, time.Second*5)
			if err != nil {
				if isTimeoutError(err) && !a.Done() {
					continue
				}
				return
			}
			_, err = xio.WriteFull(upConn, buf[:n])
			if err != nil {
				return
			}
			a.Reset()
		}
	}()

	go func() {
		bufPtr := udpPool.Get().(*[]byte)
		defer udpPool.Put(bufPtr)
		defer close(upClosed)

		buf := *bufPtr
		for {
			n, err := readConnWithTimeout(upConn, buf, time.Second*5)
			if err != nil {
				if isTimeoutError(err) && !a.Done() {
					continue
				}
				return
			}
			_, err = xio.WriteFull(downConn, buf[:n])
			if err != nil {
				return
			}
			a.Reset()
		}
	}()

	var waitFor chan struct{}
	select {
	case <-downClosed:
		klog.V(4).InfoS("Client close connection", constants.LogFieldRequestID, reqID)
		_ = upConn.Close()
		waitFor = upClosed
	case <-upClosed:
		klog.V(4).InfoS("Server close connection", constants.LogFieldRequestID, reqID)
		_ = downConn.CloseRead()
		waitFor = downClosed
	}

	<-waitFor
}
