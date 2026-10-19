package auth

import (
	"sync"
	"time"
)

// Memory is the in-process Cache. Putting a token drops the ones whose
// refresh token has expired, so it holds no more than the live ones.
type Memory struct {
	mu sync.Mutex
	m  map[string]Token
}

// NewMemory returns an empty cache.
func NewMemory() *Memory { return &Memory{m: map[string]Token{}} }

func (c *Memory) Get(key string) (Token, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return t, ok
}

func (c *Memory) Put(key string, t Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, old := range c.m {
		if !old.RefreshExpiry.IsZero() && old.RefreshExpiry.Before(now) {
			delete(c.m, k)
		}
	}
	c.m[key] = t
}

func (c *Memory) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}
