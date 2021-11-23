package main

import (
	"sync"
)

type connTrack struct {
	mu    sync.RWMutex
	items map[string]chan []byte
}

func newConnTrack() connTrack {
	return connTrack{items: map[string]chan []byte{}}
}

func (c *connTrack) Get(key string) (chan []byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.items[key]
	return item, ok
}

func (c *connTrack) Set(key string, item chan []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = item
}

func (c *connTrack) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}
