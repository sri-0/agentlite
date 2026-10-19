package auth

import "time"

// Token is an exchanged token as a cache keeps it: the access token to send
// and the refresh token that renews it, with when each expires.
type Token struct {
	Access        string
	AccessExpiry  time.Time
	Refresh       string
	RefreshExpiry time.Time // zero when the endpoint did not say
}

// Cache keeps tokens by key. Memory is the one there is; a cache that
// survives restarts (Valkey with AES-GCM, Vault) would implement the same.
type Cache interface {
	Get(key string) (Token, bool)
	Put(key string, t Token)
	Delete(key string)
}
