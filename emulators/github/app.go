package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// App is a GitHub App, which signs its own JWTs with its private key.
type App struct {
	ID  int64
	Key *rsa.PrivateKey
}

// PrivateKeyPEM is the App's private key as GitHub hands it out.
func (a *App) PrivateKeyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(a.Key)})
}

// Installation is an App installed on an account, seeing all its
// repositories.
type Installation struct {
	ID      int64
	App     *App
	Account *Account
}

type installationToken struct {
	installation *Installation
	expires      time.Time
}

// NewApp registers an App with a fresh key.
func (e *Emulator) NewApp(id int64) *App {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	a := &App{ID: id, Key: k}
	e.apps[id] = a
	return a
}

// Install installs an App on an account.
func (e *Emulator) Install(a *App, on *Account) *Installation {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := &Installation{ID: e.id(), App: a, Account: on}
	e.installations[in.ID] = in
	return in
}

var errBadJWT = errors.New("A JSON web token could not be decoded")

// appFromJWT checks an App's JWT as GitHub does: RS256, signed with the
// App's key, issued by its ID, and expiring no more than 10 minutes after it
// was issued.
func (e *Emulator) appFromJWT(r *http.Request) (*App, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, errBadJWT
	}
	var app *App
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		iss, err := t.Claims.GetIssuer()
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(iss, 10, 64)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		app = e.apps[id]
		e.mu.Unlock()
		if app == nil {
			return nil, errors.New("unknown app")
		}
		return &app.Key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuedAt(), jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(e.clock.Now), jwt.WithLeeway(time.Minute))
	if err != nil || !tok.Valid {
		return nil, errBadJWT
	}
	iat, _ := tok.Claims.GetIssuedAt()
	exp, _ := tok.Claims.GetExpirationTime()
	if iat == nil || exp.Sub(iat.Time) > 10*time.Minute+time.Minute {
		return nil, errors.New("'Expiration time' claim ('exp') is too far in the future")
	}
	return app, nil
}

func (e *Emulator) listInstallations(w http.ResponseWriter, r *http.Request) {
	app, err := e.appFromJWT(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	e.mu.Lock()
	var out []map[string]any
	for _, in := range e.installations {
		if in.App == app {
			out = append(out, map[string]any{
				"id":                   in.ID,
				"app_id":               app.ID,
				"account":              accountJSON(in.Account),
				"target_type":          in.Account.Type,
				"repository_selection": "all",
			})
		}
	}
	e.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (e *Emulator) createToken(w http.ResponseWriter, r *http.Request) {
	app, err := e.appFromJWT(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.installations[id]
	if in == nil || in.App != app {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	var b [20]byte
	_, _ = rand.Read(b[:])
	tok := "ghs_" + hex.EncodeToString(b[:])
	exp := e.clock.Now().Add(time.Hour)
	e.tokens[tok] = installationToken{installation: in, expires: exp}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":                tok,
		"expires_at":           exp.UTC().Format(time.RFC3339),
		"permissions":          map[string]string{"actions": "read", "contents": "read", "pull_requests": "read", "metadata": "read"},
		"repository_selection": "all",
	})
}

// installation is the installation a request's token belongs to.
func (e *Emulator) installation(r *http.Request) *Installation {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "token ")
	if !ok {
		tok, ok = strings.CutPrefix(h, "Bearer ")
	}
	if !ok {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tokens[tok]
	if !ok || !e.clock.Now().Before(t.expires) {
		return nil
	}
	return t.installation
}
