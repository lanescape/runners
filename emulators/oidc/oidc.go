// Package oidc is an OpenID Connect issuer with its own signing keys and
// published key set, for emulating GitHub's Actions tokens and Google's ID
// tokens. Tests mint tokens with any claims, including wrong ones, and can
// sign with a key the issuer doesn't publish.
package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jonboulle/clockwork"
)

// Key is a signing key and its key ID.
type Key struct {
	ID      string
	Private *rsa.PrivateKey
}

// NewKey makes an RSA signing key.
func NewKey() Key {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	return Key{ID: hex.EncodeToString(id[:]), Private: k}
}

// Issuer is a running issuer.
type Issuer struct {
	// URL is the issuer, as its tokens' iss claim names it.
	URL string
	// JWKSURL is where its keys are published.
	JWKSURL string

	srv      *httptest.Server
	jwksPath string
	clock    clockwork.Clock
	lifetime time.Duration

	mu        sync.Mutex
	key       Key
	published []Key
}

// Option configures an Issuer.
type Option func(*Issuer)

// WithJWKSPath sets where the key set is served; GitHub's is
// /.well-known/jwks.
func WithJWKSPath(p string) Option { return func(i *Issuer) { i.jwksPath = p } }

// WithClock sets the clock that stamps iat, nbf and exp.
func WithClock(c clockwork.Clock) Option { return func(i *Issuer) { i.clock = c } }

// WithLifetime sets how long minted tokens are valid.
func WithLifetime(d time.Duration) Option { return func(i *Issuer) { i.lifetime = d } }

// New starts an issuer. Close it when done.
func New(opts ...Option) *Issuer {
	i := &Issuer{jwksPath: "/.well-known/jwks", clock: clockwork.NewRealClock(), lifetime: 5 * time.Minute}
	for _, o := range opts {
		o(i)
	}
	i.key = NewKey()
	i.published = []Key{i.key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET "+i.jwksPath, i.jwks)
	i.srv = httptest.NewServer(mux)
	i.URL = i.srv.URL
	i.JWKSURL = i.srv.URL + i.jwksPath
	return i
}

// Close stops the issuer.
func (i *Issuer) Close() { i.srv.Close() }

// Rotate starts signing with a new key, publishing it beside the old ones.
func (i *Issuer) Rotate() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.key = NewKey()
	i.published = append(i.published, i.key)
}

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                i.URL,
		"jwks_uri":                              i.JWKSURL,
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported":                      []string{"sub", "aud", "exp", "iat", "iss", "jti", "nbf"},
	})
}

func (i *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	var set jose.JSONWebKeySet
	for _, k := range i.published {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.Private.PublicKey, KeyID: k.ID, Algorithm: "RS256", Use: "sig"})
	}
	i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

// Mint signs a token with the issuer's current key. It sets iss, iat, nbf,
// exp and jti unless claims sets them; a claim set to nil is left out.
func (i *Issuer) Mint(claims map[string]any) string {
	i.mu.Lock()
	k := i.key
	i.mu.Unlock()
	return i.MintWith(k, claims)
}

// MintWith signs a token with any key, such as one the issuer doesn't
// publish.
func (i *Issuer) MintWith(k Key, claims map[string]any) string {
	now := i.clock.Now()
	var jti [16]byte
	_, _ = rand.Read(jti[:])
	full := map[string]any{
		"iss": i.URL,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(i.lifetime).Unix(),
		"jti": hex.EncodeToString(jti[:]),
	}
	maps.Copy(full, claims)
	for name, v := range full {
		if v == nil {
			delete(full, name)
		}
	}
	return Sign(k, full)
}

// Sign signs any claims with k, as RS256.
func Sign(k Key, claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(fmt.Sprintf("oidc: claims: %v", err))
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k.Private},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", k.ID))
	if err != nil {
		panic(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return s
}
