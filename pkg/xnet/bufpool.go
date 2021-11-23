package xnet

import (
	"sync"
)

func newBufferPool(size int) sync.Pool {
	return sync.Pool{
		New: func() interface{} {
			return make([]byte, size)
		},
	}
}
