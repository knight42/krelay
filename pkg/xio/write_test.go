package xio

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeWriter struct {
	batchSize int
	buf       []byte
}

func (f *fakeWriter) Write(p []byte) (n int, err error) {
	size := len(p)
	if size > f.batchSize {
		size = f.batchSize
	}
	f.buf = append(f.buf, p[:size]...)
	return size, nil
}

func TestWriteFull(t *testing.T) {
	w := &fakeWriter{batchSize: 2}
	r := require.New(t)
	const msg = "0123456789"
	n, err := WriteFull(w, []byte(msg))
	r.NoError(err)
	r.Len(msg, n)
	r.Equal(msg, string(w.buf))
}
