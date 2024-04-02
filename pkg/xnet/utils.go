package xnet

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

func readConnWithTimeout(c net.Conn, buf []byte, timeout time.Duration) (n int, err error) {
	setReadDeadline(c, timeout)
	return c.Read(buf)
}

func isTimeoutError(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}

func readFullWithTimeout(r io.Reader, buf []byte, timeout time.Duration) (int, error) {
	conn, ok := r.(net.Conn)
	if ok {
		setReadDeadline(conn, timeout)
		return io.ReadFull(conn, buf)
	}
	return io.ReadFull(r, buf)
}

func setReadDeadline(c net.Conn, timeout time.Duration) {
	if timeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(timeout))
	}
}

func JoinHostPort(host string, port uint16) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

func IsClosedConnectionError(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func newBufferPool(size int) sync.Pool {
	return sync.Pool{
		New: func() any {
			buf := make([]byte, size)
			return &buf
		},
	}
}
