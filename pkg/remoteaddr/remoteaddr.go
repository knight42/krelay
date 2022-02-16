package remoteaddr

import (
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/knight42/krelay/pkg/xnet"
)

type Getter interface {
	Get() (xnet.Addr, error)
}

func NewStaticAddr(addr xnet.Addr) Getter {
	return &staticAddr{addr: addr}
}

func NewDynamicAddr(cs kubernetes.Interface, ns, selector string) (Getter, error) {
	ret := &dynamicAddr{
		podCli:   cs.CoreV1().Pods(ns),
		selector: selector,
	}
	err := ret.init()
	if err != nil {
		return nil, fmt.Errorf("init pod ip: %w", err)
	}
	return ret, nil
}
