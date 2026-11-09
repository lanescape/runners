package cache

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
)

// IAMSigner signs V4 URLs for the cache's bucket as the service's own
// account, through IAM's signBlob, so the service holds no key
// (specs/0006-cache.md, Storage and billing). A URL it signs acts as that
// account, for one object and one method, until it expires.
type IAMSigner struct {
	bucket  string
	account string
	iam     *iamcredentials.Service
	// Hostname and Insecure point URLs at an emulator in tests; in
	// production they're Cloud Storage's own, over HTTPS.
	Hostname string
	Insecure bool
}

// NewIAMSigner returns a signer for bucket, as account. opts are passed to
// the IAM credentials client, such as an emulator's endpoint in tests.
func NewIAMSigner(ctx context.Context, bucket, account string, opts ...option.ClientOption) (*IAMSigner, error) {
	if bucket == "" || account == "" {
		return nil, errors.New("cache: a signer needs its bucket and account")
	}
	svc, err := iamcredentials.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("iam credentials client: %w", err)
	}
	return &IAMSigner{bucket: bucket, account: account, iam: svc}, nil
}

// SignedURL implements Signer.
func (s *IAMSigner) SignedURL(ctx context.Context, object, method string, query url.Values, headers []string, expires time.Time) (string, error) {
	return storage.SignedURL(s.bucket, object, &storage.SignedURLOptions{
		GoogleAccessID:  s.account,
		SignBytes:       func(b []byte) ([]byte, error) { return s.sign(ctx, b) },
		Method:          method,
		Expires:         expires,
		Headers:         headers,
		QueryParameters: query,
		Scheme:          storage.SigningSchemeV4,
		Style:           storage.PathStyle(),
		Hostname:        s.Hostname,
		Insecure:        s.Insecure,
	})
}

func (s *IAMSigner) sign(ctx context.Context, payload []byte) ([]byte, error) {
	res, err := s.iam.Projects.ServiceAccounts.SignBlob("projects/-/serviceAccounts/"+s.account,
		&iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("signing with IAM: %w", err)
	}
	return base64.StdEncoding.DecodeString(res.SignedBlob)
}
