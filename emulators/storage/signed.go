package storage

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What a V4 signed URL carries, as Cloud Storage reads it.
const (
	signingAlgorithm = "GOOG4-RSA-SHA256"
	signedDateFormat = "20060102T150405Z"
	// maxSignedExpiry is the longest a V4 signed URL may be valid: a week.
	maxSignedExpiry = 7 * 24 * time.Hour
)

// TrustSigner accepts V4 signed URLs signed by email's key, as Cloud Storage
// accepts those of an account with access to the bucket. A URL signed by an
// account the emulator doesn't trust is refused, as one whose account has
// no access is.
func (e *Emulator) TrustSigner(email string, key *rsa.PublicKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.signers[email] = key
}

func errAccessDenied(msg string) *apiError {
	return &apiError{http.StatusForbidden, "forbidden", "AccessDenied", msg}
}

func errSignature() *apiError {
	return &apiError{http.StatusForbidden, "forbidden", "SignatureDoesNotMatch",
		"The request signature we calculated does not match the signature you provided. Check your Google secret key and signing method."}
}

func errExpired() *apiError {
	return &apiError{http.StatusBadRequest, "invalid", "ExpiredToken", "Invalid argument."}
}

func errAuthorization(msg string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid", "AuthorizationQueryParametersError", msg}
}

// encodePathV4 escapes a path as a V4 canonical request has it: each
// segment query-escaped, with spaces as %20.
func encodePathV4(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.QueryEscape(s)
	}
	return strings.ReplaceAll(strings.Join(segments, "/"), "+", "%20")
}

// verifySigned checks a request made with a V4 signed URL for an object, as
// Cloud Storage does: its algorithm, credential, date and expiry, then its
// signature over the canonical request, which covers its method, path,
// query and every signed header, with the key of the account that signed
// it. The request must carry each signed header with the value signed.
func (e *Emulator) verifySigned(r *http.Request, bucketName, objectName string) error {
	q := r.URL.Query()
	if q.Get("X-Goog-Algorithm") != signingAlgorithm {
		return errAuthorization("X-Goog-Algorithm must be " + signingAlgorithm)
	}
	credential := q.Get("X-Goog-Credential")
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[2] != "auto" || parts[3] != "storage" || parts[4] != "goog4_request" {
		return errAuthorization("X-Goog-Credential isn't <account>/<date>/auto/storage/goog4_request")
	}
	account, day := parts[0], parts[1]
	date, err := time.Parse(signedDateFormat, q.Get("X-Goog-Date"))
	if err != nil || date.Format("20060102") != day {
		return errAuthorization("X-Goog-Date is missing, malformed, or not the credential's day")
	}
	secs, err := strconv.Atoi(q.Get("X-Goog-Expires"))
	if err != nil || secs < 1 || time.Duration(secs)*time.Second > maxSignedExpiry {
		return errAuthorization("X-Goog-Expires must be between 1 and 604800 seconds")
	}
	signature, err := hex.DecodeString(q.Get("X-Goog-Signature"))
	if err != nil || len(signature) == 0 {
		return errAuthorization("X-Goog-Signature is missing or isn't hex")
	}
	names := strings.Split(q.Get("X-Goog-SignedHeaders"), ";")
	if !containsString(names, "host") {
		return errAuthorization("X-Goog-SignedHeaders must include host")
	}

	// The canonical request.
	var b strings.Builder
	b.WriteString(r.Method + "\n")
	b.WriteString("/" + encodePathV4(bucketName+"/"+objectName) + "\n")
	cq := url.Values{}
	for k, v := range q {
		if k != "X-Goog-Signature" {
			cq[k] = v
		}
	}
	b.WriteString(strings.ReplaceAll(cq.Encode(), "+", "%20") + "\n")
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	payload := "UNSIGNED-PAYLOAD"
	for _, n := range sorted {
		var v string
		if n == "host" {
			v = r.Host
			if h, _, err := net.SplitHostPort(r.Host); err == nil {
				v = h
			}
		} else {
			vs, ok := r.Header[http.CanonicalHeaderKey(n)]
			if !ok {
				return errSignature()
			}
			v = strings.Join(vs, ",")
		}
		v = strings.Join(strings.Fields(v), " ")
		b.WriteString(n + ":" + v + "\n")
		if n == "x-goog-content-sha256" {
			payload = v
		}
	}
	b.WriteString("\n" + strings.Join(names, ";") + "\n")
	b.WriteString(payload)

	sum := sha256.Sum256([]byte(b.String()))
	toSign := signingAlgorithm + "\n" + q.Get("X-Goog-Date") + "\n" + strings.Join(parts[1:], "/") + "\n" + hex.EncodeToString(sum[:])
	e.mu.Lock()
	key := e.signers[account]
	now := e.clock.Now()
	e.mu.Unlock()
	if key == nil {
		return errSignature()
	}
	digest := sha256.Sum256([]byte(toSign))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return errSignature()
	}
	if !now.Before(date.Add(time.Duration(secs) * time.Second)) {
		return errExpired()
	}
	return nil
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
