package remoteaddr

import (
	"github.com/knight42/krelay/pkg/xnet"
)

type staticAddr struct {
	addr xnet.Addr
}

var _ Getter = (*staticAddr)(nil)

func (s *staticAddr) Get() (xnet.Addr, error) {
	return s.addr, nil
}
