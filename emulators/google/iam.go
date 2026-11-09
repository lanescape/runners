package google

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"google.golang.org/api/option"

	"github.com/lanescape/runners/emulators/fault"
)

// IAM emulates IAM's Service Account Credentials API, as
// google.golang.org/api/iamcredentials/v1 calls it: signBlob, which signs
// bytes with a service account's Google-managed key, for a caller allowed
// to. Only the accounts a test adds may be signed as; any other is refused,
// as one the caller has no grant on is.
type IAM struct {
	// URL is the emulator's base address, with no trailing slash.
	URL string

	srv    *httptest.Server
	faults fault.Injector

	mu   sync.Mutex
	keys map[string]signingKey
}

type signingKey struct {
	id  string
	key *rsa.PrivateKey
}

// NewIAM starts an IAM emulator. Close it when done.
func NewIAM() *IAM {
	i := &IAM{keys: map[string]signingKey{}}
	i.srv = httptest.NewServer(i.faults.Wrap(http.HandlerFunc(i.serve)))
	i.URL = i.srv.URL
	return i
}

// Close stops the emulator.
func (i *IAM) Close() { i.srv.Close() }

// Faults is where tests make requests fail.
func (i *IAM) Faults() *fault.Injector { return &i.faults }

// ClientOptions are what google.golang.org/api/iamcredentials/v1 needs to
// talk to the emulator instead of Google.
func (i *IAM) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(i.URL + "/"), option.WithoutAuthentication()}
}

// AddAccount makes a service account's key, and lets callers sign as it.
// It returns the key's public half, which a verifier trusts.
func (i *IAM) AddAccount(email string) *rsa.PublicKey {
	i.mu.Lock()
	defer i.mu.Unlock()
	if k, ok := i.keys[email]; ok {
		return &k.key.PublicKey
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	var id [20]byte
	_, _ = rand.Read(id[:])
	i.keys[email] = signingKey{id: hex.EncodeToString(id[:]), key: k}
	return &k.PublicKey
}

// Key is an added account's public key, or nil.
func (i *IAM) Key(email string) *rsa.PublicKey {
	i.mu.Lock()
	defer i.mu.Unlock()
	if k, ok := i.keys[email]; ok {
		return &k.key.PublicKey
	}
	return nil
}

// GoogleError answers as Google's JSON APIs do with an error, such as 503
// UNAVAILABLE or 429 RESOURCE_EXHAUSTED, for fault rules.
func GoogleError(code int, status, message string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { writeGoogleError(w, code, status, message) }
}

func writeGoogleError(w http.ResponseWriter, code int, status, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": message, "status": status}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

const signBlobPrefix = "/v1/projects/-/serviceAccounts/"

func (i *IAM) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, signBlobPrefix)
	email, found := strings.CutSuffix(rest, ":signBlob")
	if !ok || !found || r.Method != http.MethodPost || email == "" || strings.Contains(email, "/") {
		writeGoogleError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return
	}
	var req struct {
		Payload []byte `json:"payload"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid JSON payload received.")
		return
	}
	i.mu.Lock()
	k, ok := i.keys[email]
	i.mu.Unlock()
	if !ok {
		writeGoogleError(w, http.StatusForbidden, "PERMISSION_DENIED",
			"Permission 'iam.serviceAccounts.signBlob' denied on resource (or it may not exist).")
		return
	}
	if len(req.Payload) == 0 {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Request contains an invalid argument.")
		return
	}
	sum := sha256.Sum256(req.Payload)
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, sum[:])
	if err != nil {
		writeGoogleError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keyId": k.id, "signedBlob": base64.StdEncoding.EncodeToString(sig)})
}
