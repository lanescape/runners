package google_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/google"
	gcsemu "github.com/lanescape/runners/emulators/storage"
)

const account = "lanescape-runners@lanescape-dev.iam.gserviceaccount.com"

func iamClient(t *testing.T) (*google.IAM, *iamcredentials.Service) {
	t.Helper()
	emu := google.NewIAM()
	t.Cleanup(emu.Close)
	svc, err := iamcredentials.NewService(context.Background(), emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	return emu, svc
}

func signBlob(svc *iamcredentials.Service, email string, payload []byte) ([]byte, error) {
	res, err := svc.Projects.ServiceAccounts.SignBlob("projects/-/serviceAccounts/"+email,
		&iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)}).Do()
	if err != nil {
		return nil, err
	}
	if res.KeyId == "" {
		return nil, errors.New("no key ID")
	}
	return base64.StdEncoding.DecodeString(res.SignedBlob)
}

func TestSignBlob(t *testing.T) {
	emu, svc := iamClient(t)
	pub := emu.AddAccount(account)
	if emu.Key(account) != pub || emu.AddAccount(account) != pub {
		t.Fatal("an account's key changed")
	}
	sig, err := signBlob(svc, account, []byte("to sign"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("to sign"))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature doesn't verify: %v", err)
	}
	var ge *googleapi.Error
	if _, err := signBlob(svc, "other@lanescape-dev.iam.gserviceaccount.com", []byte("x")); !errors.As(err, &ge) || ge.Code != http.StatusForbidden {
		t.Fatalf("signing as an account with no grant: %v, want 403", err)
	}
	if emu.Key("other@lanescape-dev.iam.gserviceaccount.com") != nil {
		t.Fatal("an unknown account has a key")
	}
	emu.Faults().Add(fault.Rule{Times: 1, Respond: google.GoogleError(http.StatusServiceUnavailable, "UNAVAILABLE", "try again")})
	if _, err := signBlob(svc, account, []byte("x")); !errors.As(err, &ge) || ge.Code != http.StatusServiceUnavailable {
		t.Fatalf("signing with a fault: %v, want 503", err)
	}
}

// TestSignedURLsThroughIAM signs URLs as the service does, with the
// storage SDK and IAM's signBlob, and uses them as a job does, with plain
// HTTP: a resumable upload in chunks, then ranged reads.
func TestSignedURLsThroughIAM(t *testing.T) {
	iam, svc := iamClient(t)
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("cache")
	gcs.TrustSigner(account, iam.AddAccount(account))
	sign := func(method, object string, headers []string) string {
		t.Helper()
		u, err := storage.SignedURL("cache", object, &storage.SignedURLOptions{
			GoogleAccessID: account,
			SignBytes:      func(b []byte) ([]byte, error) { return signBlob(svc, account, b) },
			Method:         method,
			Expires:        time.Now().Add(15 * time.Minute),
			Scheme:         storage.SigningSchemeV4,
			Headers:        headers,
			Hostname:       strings.TrimPrefix(gcs.URL, "http://"),
			Insecure:       true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	send := func(method, u string, h http.Header, body []byte) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, u, bytes.NewReader(body))
		for k, v := range h {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	start := sign(http.MethodPost, "uploads/5001/r1", []string{"x-goog-resumable:start", "x-goog-if-generation-match:0"})
	resp := send(http.MethodPost, start, http.Header{"X-Goog-Resumable": {"start"}, "X-Goog-If-Generation-Match": {"0"}}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	session := resp.Header.Get("Location")
	if resp := send(http.MethodPut, session, http.Header{"Content-Range": {"bytes 0-3/*"}}, []byte("abcd")); resp.StatusCode != 308 {
		t.Fatalf("chunk = %d", resp.StatusCode)
	}
	if resp := send(http.MethodPut, session, http.Header{"Content-Range": {"bytes 4-7/8"}}, []byte("efgh")); resp.StatusCode != http.StatusOK {
		t.Fatalf("last chunk = %d", resp.StatusCode)
	}
	get := sign(http.MethodGet, "uploads/5001/r1", nil)
	resp = send(http.MethodGet, get, http.Header{"Range": {"bytes=2-5"}}, nil)
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(data) != "cdef" {
		t.Fatalf("ranged read = %d %q", resp.StatusCode, data)
	}
}
