package xio

import (
	"io"
)

func WriteFull(w io.Writer, p []byte) (n int, err error) {
	size := len(p)
	for n < size && err == nil {
		var nw int
		nw, err = w.Write(p[n:])
		n += nw
	}
	if n == size {
		return n, nil
	}
	return n, io.ErrShortWrite
}
