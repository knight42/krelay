package tcp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func NewTLSServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *url.URL, uint16) {
	server := httptest.NewTLSServer(handler)
	u, _ := url.Parse(server.URL)
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return server, u, uint16(port)
}

func NewTCPServer(t *testing.T, handler func(conn net.Conn)) net.Listener {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := l.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		handler(c)
	}()
	return l
}
