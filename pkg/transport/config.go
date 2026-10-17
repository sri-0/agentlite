package transport

// Env is the transport's configuration from the environment (TRANSPORT_*).
type Env struct {
	// SensitiveHeaders are a caller's headers Forward never passes on: who
	// the caller is. An upstream gets credentials from its own client
	// (Config), or from rewrite when the caller's should go through.
	SensitiveHeaders []string `env:"SENSITIVE_HEADERS, default=Authorization,Cookie,X-Api-Key,X-Forwarded-Client-Cert"`
}
