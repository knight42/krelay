package main

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

type fakeStream struct {
	written []byte
	closed  atomic.Bool
}

func (s *fakeStream) Read([]byte) (int, error) { return 0, io.EOF }
func (s *fakeStream) Write(p []byte) (int, error) {
	s.written = append(s.written, p...)
	return len(p), nil
}
func (s *fakeStream) Close() error         { s.closed.Store(true); return nil }
func (s *fakeStream) Reset() error         { return nil }
func (s *fakeStream) Headers() http.Header { return nil }
func (s *fakeStream) Identifier() uint32   { return 0 }

type fakeConn struct {
	closeCh        chan bool
	createCount    atomic.Int32
	createErr      error
	createWriteErr bool
}

func newFakeConn() *fakeConn {
	return &fakeConn{closeCh: make(chan bool)}
}

func (c *fakeConn) CreateStream(_ http.Header) (httpstream.Stream, error) {
	if c.createErr != nil {
		return nil, c.createErr
	}
	c.createCount.Add(1)
	s := &fakeStream{}
	if c.createWriteErr {
		return &errorStream{fakeStream: s}, nil
	}
	return s, nil
}

func (c *fakeConn) Close() error                       { return nil }
func (c *fakeConn) CloseChan() <-chan bool             { return c.closeCh }
func (c *fakeConn) SetIdleTimeout(time.Duration)       {}
func (c *fakeConn) RemoveStreams(...httpstream.Stream) {}

type errorStream struct {
	*fakeStream
}

func (s *errorStream) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func TestSendHeartbeats_CloseChan(t *testing.T) {
	conn := newFakeConn()
	close(conn.closeCh)

	done := make(chan struct{})
	go func() {
		sendHeartbeats(conn, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sendHeartbeats did not return after CloseChan was closed")
	}
	assert.Equal(t, int32(0), conn.createCount.Load())
}

func TestSendHeartbeats_SendsHeartbeats(t *testing.T) {
	conn := newFakeConn()

	done := make(chan struct{})
	go func() {
		sendHeartbeats(conn, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(55 * time.Millisecond)
	close(conn.closeCh)
	<-done

	// createStream is called twice per heartbeat (error stream + data stream)
	count := conn.createCount.Load()
	assert.GreaterOrEqual(t, count, int32(4), "expected at least 2 heartbeats (4 streams), got %d streams", count)
}

func TestSendHeartbeats_CreateStreamError(t *testing.T) {
	conn := newFakeConn()
	conn.createErr = errors.New("connection refused")

	done := make(chan struct{})
	go func() {
		sendHeartbeats(conn, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sendHeartbeats did not return after CreateStream error")
	}
}

func TestSendHeartbeats_WriteError(t *testing.T) {
	conn := newFakeConn()
	conn.createWriteErr = true

	done := make(chan struct{})
	go func() {
		sendHeartbeats(conn, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sendHeartbeats did not return after write error")
	}
}
