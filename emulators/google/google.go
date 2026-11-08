// Package google emulates Google's ID token issuer, as Cloud Scheduler and
// Pub/Sub push subscriptions sign their calls: tokens for a service
// account's email, with the audience the caller was configured with.
package google

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/lanescape/runners/emulators/oidc"
)

// Issuer is a running emulated Google issuer.
type Issuer struct {
	*oidc.Issuer
}

// New starts an issuer, publishing its keys where Google does.
func New(opts ...oidc.Option) *Issuer {
	opts = append([]oidc.Option{oidc.WithJWKSPath("/oauth2/v3/certs"), oidc.WithLifetime(time.Hour)}, opts...)
	return &Issuer{oidc.New(opts...)}
}

// accountID is a stable numeric ID for an account, as Google gives each
// service account.
func accountID(email string) string {
	sum := sha256.Sum256([]byte(email))
	return strconv.FormatUint(binary.BigEndian.Uint64(sum[:8])>>1, 10)
}

// Claims are a service account's token's claims, which tests may change
// before minting.
func (i *Issuer) Claims(email, audience string) map[string]any {
	id := accountID(email)
	return map[string]any{
		"aud":            audience,
		"azp":            id,
		"sub":            id,
		"email":          email,
		"email_verified": true,
	}
}

// Token mints a service account's token for audience.
func (i *Issuer) Token(email, audience string) string {
	return i.Mint(i.Claims(email, audience))
}
