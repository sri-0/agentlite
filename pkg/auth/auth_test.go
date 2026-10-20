package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

const issuer = "http://issuer.test/realms/agentlite"

// keys is a signing key and a JWKS server that publishes it.
type keys struct {
	key *rsa.PrivateKey
	srv *httptest.Server
}

func newKeys(t *testing.T) *keys {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	b64 := base64.RawURLEncoding.EncodeToString
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
		"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
	}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(jwks) }))
	t.Cleanup(srv.Close)
	return &keys{key: key, srv: srv}
}

// sign makes an RS256 JWT of claims with key.
func sign(key *rsa.PrivateKey, claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	head, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	unsigned := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(unsigned))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return unsigned + "." + b64(sig)
}

// claims are a valid access token's, with changes.
func claims(change map[string]any) map[string]any {
	c := map[string]any{
		"iss": issuer, "sub": "u-alice", "aud": []string{"opensearch", "agentlite", "controlsets"},
		"exp": time.Now().Add(15 * time.Minute).Unix(), "iat": time.Now().Unix(), "typ": "Bearer",
		"preferred_username": "alice", "controlset_ids": []string{"C1001", "C1000"},
		"realm_access":    map[string]any{"roles": []string{"offline_access", "analyst"}},
		"resource_access": map[string]any{"agentlite": map[string]any{"roles": []string{"operator"}}},
		"groups":          []string{"/CONTROLSET/C1000"},
	}
	for k, v := range change {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func newVerifier(t *testing.T, k *keys) *Verifier {
	t.Helper()
	v, err := New(t.Context(), Config{IssuerURL: issuer, JWKSURL: k.srv.URL, Audience: "agentlite", ControlsetClaim: "controlset_ids"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestVerify: an access token for this server is accepted with its user,
// control sets, roles and groups; anything else is refused.
func TestVerify(t *testing.T) {
	k := newKeys(t)
	v := newVerifier(t, k)
	p, err := v.Verify(t.Context(), sign(k.key, claims(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "u-alice" || p.Username != "alice" || strings.Join(p.Controlsets, ",") != "C1001,C1000" || p.Token == "" ||
		strings.Join(p.Roles, ",") != "offline_access,analyst,agentlite:operator" || strings.Join(p.Groups, ",") != "/CONTROLSET/C1000" {
		t.Fatalf("principal %+v", p)
	}
	if p, err := v.Verify(t.Context(), sign(k.key, claims(map[string]any{"controlset_ids": nil, "realm_access": nil, "resource_access": nil, "groups": nil}))); err != nil || len(p.Controlsets) != 0 || len(p.Roles) != 0 {
		t.Fatalf("no control sets: got %+v, %v; want a principal that holds none", p, err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, raw := range map[string]string{
		"another audience":    sign(k.key, claims(map[string]any{"aud": "opensearch"})),
		"another issuer":      sign(k.key, claims(map[string]any{"iss": "http://evil.test/realms/agentlite"})),
		"expired":             sign(k.key, claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})),
		"a refresh token":     sign(k.key, claims(map[string]any{"typ": "Refresh"})),
		"an ID token":         sign(k.key, claims(map[string]any{"typ": "ID"})),
		"no username":         sign(k.key, claims(map[string]any{"preferred_username": nil})),
		"a single string":     sign(k.key, claims(map[string]any{"controlset_ids": "C1000"})),
		"another key":         sign(other, claims(nil)),
		"not a JWT":           "abc.def.ghi",
		"unsigned (alg none)": strings.Join(strings.Split(sign(k.key, claims(nil)), ".")[:2], ".") + ".",
	} {
		if _, err := v.Verify(t.Context(), raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestMiddleware: every path but the open ones needs a valid bearer token,
// and a request let in carries its principal.
func TestMiddleware(t *testing.T) {
	k := newKeys(t)
	e := echo.New()
	e.Use(newVerifier(t, k).Middleware("/health"))
	e.GET("/health", func(c *echo.Context) error { return c.String(http.StatusOK, "ok") })
	e.GET("/x", func(c *echo.Context) error {
		p := From(c.Request().Context())
		return c.String(http.StatusOK, p.Username+" "+strings.Join(p.Controlsets, ","))
	})
	do := func(path, authz string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	if w := do("/health", ""); w.Code != http.StatusOK {
		t.Fatalf("/health: %d", w.Code)
	}
	for name, authz := range map[string]string{
		"none": "", "basic": "Basic YWRtaW46YWRtaW4=", "empty bearer": "Bearer ",
		"bad token": "Bearer abc.def.ghi", "expired": "Bearer " + sign(k.key, claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})),
	} {
		w := do("/x", authz)
		if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("WWW-Authenticate"))
		}
	}
	if w := do("/x", "Bearer "+sign(k.key, claims(nil))); w.Code != http.StatusOK || w.Body.String() != "alice C1001,C1000" {
		t.Fatalf("valid: %d %q", w.Code, w.Body.String())
	}
}

// TestControlset: every POST but the exempt ones takes exactly one control
// set, one of the principal's, which the principal then carries; other
// methods pass.
func TestControlset(t *testing.T) {
	k := newKeys(t)
	e := echo.New()
	e.Use(newVerifier(t, k).Middleware(), Controlset(ControlsetConfig{Header: "X-Control-Set", ExemptPaths: []string{"/open", "/mcp/:id"}}))
	echoControlset := func(c *echo.Context) error { return c.String(http.StatusOK, From(c.Request().Context()).Controlset) }
	e.POST("/create", echoControlset)
	e.GET("/create", echoControlset)
	e.POST("/open", echoControlset)
	e.POST("/mcp/:id", echoControlset)
	token := "Bearer " + sign(k.key, claims(nil))
	for _, tc := range []struct {
		name, method, path string
		headers            []string
		code               int
		body               string
	}{
		{"one of theirs", http.MethodPost, "/create", []string{"C1000"}, http.StatusOK, "C1000"},
		{"not theirs", http.MethodPost, "/create", []string{"C1015"}, http.StatusForbidden, ""},
		{"none", http.MethodPost, "/create", nil, http.StatusBadRequest, ""},
		{"blank", http.MethodPost, "/create", []string{" "}, http.StatusBadRequest, ""},
		{"two headers", http.MethodPost, "/create", []string{"C1000", "C1001"}, http.StatusBadRequest, ""},
		{"a list", http.MethodPost, "/create", []string{"C1000,C1001"}, http.StatusBadRequest, ""},
		{"a GET", http.MethodGet, "/create", nil, http.StatusOK, ""},
		{"an exempt path", http.MethodPost, "/open", nil, http.StatusOK, ""},
		{"an exempt route", http.MethodPost, "/mcp/slides", nil, http.StatusOK, ""},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", token)
		for _, h := range tc.headers {
			r.Header.Add("X-Control-Set", h)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		if w.Code != tc.code || (tc.code == http.StatusOK && w.Body.String() != tc.body) {
			t.Errorf("%s: %d %q", tc.name, w.Code, w.Body.String())
		}
	}
}

// TestRequireRole: a route guarded by a role lets in only a token that
// carries it, realm roles by name and client roles as client:role.
func TestRequireRole(t *testing.T) {
	k := newKeys(t)
	e := echo.New()
	e.Use(newVerifier(t, k).Middleware())
	ok := func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) }
	e.GET("/analyst", ok, RequireRole("analyst"))
	e.GET("/operator", ok, RequireRole("agentlite:operator"))
	e.GET("/admin", ok, RequireRole("admin"))
	for path, code := range map[string]int{"/analyst": http.StatusNoContent, "/operator": http.StatusNoContent, "/admin": http.StatusForbidden} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+sign(k.key, claims(nil)))
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		if w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
}
