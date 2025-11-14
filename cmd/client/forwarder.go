package main

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"k8s.io/apimachinery/pkg/util/httpstream"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/remoteaddr"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

type portForwarder struct {
	addrGetter remoteaddr.Getter
	ports      ports.PortPair
	listenAddr string

	tcpListener net.Listener
	udpListener net.PacketConn
}

func (p *portForwarder) listen() error {
	bindAddr := net.JoinHostPort(p.listenAddr, strconv.Itoa(int(p.ports.LocalPort)))
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
		lis := p.tcpListener
		defer lis.Close()

		localAddr := lis.Addr().String()
		l := slog.With(
			slog.String(constants.LogFieldProtocol, p.ports.Protocol),
			slog.String(constants.LogFieldLocalAddr, localAddr),
		)
		l.Info("Forwarding",
			slogutil.Uint16(constants.LogFieldRemotePort, p.ports.RemotePort),
		)

		for {
			select {
			case <-streamConn.CloseChan():
				return
			default:
			}

			c, err := lis.Accept()
			if err != nil {
				l.Error("Fail to accept tcp connection", slogutil.Error(err))
				return
			}

			remoteAddr, err := p.addrGetter.Get()
			if err != nil {
				l.Error("Fail to get remote address", slogutil.Error(err))
				continue
			}
			go handleTCPConn(c, streamConn, xnet.AddrPortFrom(remoteAddr, p.ports.RemotePort))
		}

	case p.udpListener != nil:
		pc := p.udpListener
		defer pc.Close()

		udpConn := &xnet.UDPConn{UDPConn: pc.(*net.UDPConn)}
		localAddr := pc.LocalAddr().String()
		l := slog.With(
			slog.String(constants.LogFieldProtocol, p.ports.Protocol),
			slog.String(constants.LogFieldLocalAddr, localAddr),
		)
		l.Info("Forwarding",
			slogutil.Uint16(constants.LogFieldRemotePort, p.ports.RemotePort),
		)
		track := newConnTrack()
		finish := make(chan string)

		go func() {
			for key := range finish {
				track.Delete(key)
				l.Debug("Remove udp conn from conntrack table",
					slog.String("key", key),
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
				l.Error("Fail to read udp packet",
					slogutil.Error(err),
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
					l.Error("Fail to get remote address",
						slogutil.Error(err),
					)
					continue
				}
				go handleUDPConn(udpConn, cliAddr, dataCh, finish, streamConn, xnet.AddrPortFrom(remoteAddr, p.ports.RemotePort))
			} else {
				dataCh = v
			}
			dataCh <- data
		}
	}
}
