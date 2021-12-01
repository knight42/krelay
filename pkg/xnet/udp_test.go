package xnet

import (
	"bytes"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadUDPFromStream(t *testing.T) {
	buf := make([]byte, 10)
	data := bytes.NewBuffer([]byte{0x00, 0x03, 0x30, 0x31, 0x32})
	r := require.New(t)
	n, err := ReadUDPFromStream(data, buf, 0)
	r.NoError(err)
	r.Equal(n, 3)
	r.Equal(buf[:n], []byte("012"))
}

func TestUDPConn(t *testing.T) {
	r := require.New(t)
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	r.NoError(err)
	defer serverConn.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svrUDPConn := &UDPConn{UDPConn: serverConn.(*net.UDPConn)}
		buf := make([]byte, 10)
		n, cliAddr, err := svrUDPConn.ReadFrom(buf)
		if err != nil {
			t.Errorf("ReadFrom: %v", err)
			return
		}
		if !assert.Equal(t, n, 3) {
			return
		}
		if !assert.Equal(t, []byte{0x0, 0x1, 0x0}, buf[:n]) {
			return
		}

		_, err = serverConn.WriteTo([]byte{0x01, 0x02, 0x03}, cliAddr)
		if err != nil {
			t.Errorf("WriteTo: %v", err)
			return
		}
	}()

	clientConn, err := net.Dial("udp", serverConn.LocalAddr().String())
	r.NoError(err)

	uc := UDPConn{UDPConn: clientConn.(*net.UDPConn)}
	_, err = uc.Write([]byte{0x00})
	r.NoError(err)
	buf := make([]byte, 10)
	n, err := uc.Read(buf)
	r.NoError(err)
	r.Equal([]byte{0x0, 0x3, 0x1, 0x2, 0x3}, buf[:n])
	wg.Wait()
}
