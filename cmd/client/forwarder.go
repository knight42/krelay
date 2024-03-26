package main

import (
	"fmt"
	"net"
	"strconv"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/remoteaddr"
	"github.com/knight42/krelay/pkg/xnet"
)

type portForwarder struct {
	addrGetter remoteaddr.Getter
	ports      ports.PortPair

	tcpListener net.Listener
	udpListener net.PacketConn
}

func newPortForwarder(addrGetter remoteaddr.Getter, pp ports.PortPair) *portForwarder {
	return &portForwarder{
		addrGetter: addrGetter,
		ports:      pp,
	}
}

func (p *portForwarder) listen(localIP string) error {
	bindAddr := net.JoinHostPort(localIP, strconv.Itoa(int(p.ports.LocalPort)))
	switch p.ports.Protocol {
	case constants.ProtocolTCP:
		l, err := net.Listen(constants.ProtocolTCP, bindAddr)
		if err != nil {
			return err
		}
		p.tcpListener = l
	case constants.ProtocolUDP:
		pc, err := net.ListenPacket(constants.ProtocolUDP, bindAddr)
		if err != nil {
			return err
		}
		p.udpListener = pc
	default:
		return fmt.Errorf("unknown protocol: %s", p.ports.Protocol)
	}
	return nil
}

func (p *portForwarder) run(streamConn httpstream.Connection) {
	switch {
	case p.tcpListener != nil:
		l := p.tcpListener
		defer l.Close()

		localAddr := l.Addr().String()
		klog.InfoS("Forwarding",
			constants.LogFieldProtocol, p.ports.Protocol,
			constants.LogFieldLocalAddr, localAddr,
			constants.LogFieldRemotePort, p.ports.RemotePort,
		)

		for {
			select {
			case <-streamConn.CloseChan():
				return
			default:
			}

			c, err := l.Accept()
			if err != nil {
				klog.ErrorS(err, "Fail to accept tcp connection",
					constants.LogFieldProtocol, p.ports.Protocol,
					constants.LogFieldLocalAddr, localAddr,
				)
				return
			}

			remoteAddr, err := p.addrGetter.Get()
			if err != nil {
				klog.ErrorS(err, "Fail to get remote address",
					constants.LogFieldProtocol, p.ports.Protocol,
					constants.LogFieldLocalAddr, localAddr,
				)
				continue
			}
			go handleTCPConn(c, streamConn, remoteAddr, p.ports.RemotePort)
		}

	case p.udpListener != nil:
		pc := p.udpListener
		defer pc.Close()

		udpConn := &xnet.UDPConn{UDPConn: pc.(*net.UDPConn)}
		localAddr := pc.LocalAddr().String()
		klog.InfoS("Forwarding",
			constants.LogFieldProtocol, p.ports.Protocol,
			constants.LogFieldLocalAddr, localAddr,
			constants.LogFieldRemotePort, p.ports.RemotePort,
		)
		track := newConnTrack()
		finish := make(chan string)

		go func() {
			for key := range finish {
				track.Delete(key)
				klog.V(4).InfoS("Remove udp conn from conntrack table",
					"key", key,
					constants.LogFieldProtocol, p.ports.Protocol,
					constants.LogFieldLocalAddr, localAddr,
				)
			}
		}()

		// https://stackoverflow.com/questions/19658052/strange-behaviour-of-golang-udp-server
		_ = udpConn.SetReadBuffer(1048576) // 1 MiB

		buf := make([]byte, constants.UDPBufferSize)
		for {
			select {
			case <-streamConn.CloseChan():
				return
			default:
			}

			n, cliAddr, err := udpConn.ReadFrom(buf)
			if err != nil {
				klog.ErrorS(err, "Fail to read udp packet",
					constants.LogFieldProtocol, p.ports.Protocol,
					constants.LogFieldLocalAddr, localAddr,
				)
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])

			key := cliAddr.String()

			var dataCh chan []byte
			v, ok := track.Get(key)
			if !ok {
				dataCh = make(chan []byte)
				track.Set(key, dataCh)
				remoteAddr, err := p.addrGetter.Get()
				if err != nil {
					klog.ErrorS(err, "Fail to get remote address",
						constants.LogFieldProtocol, p.ports.Protocol,
						constants.LogFieldLocalAddr, localAddr,
					)
					continue
				}
				go handleUDPConn(udpConn, cliAddr, dataCh, finish, streamConn, remoteAddr, p.ports.RemotePort)
			} else {
				dataCh = v
			}
			dataCh <- data
		}
	}
}
