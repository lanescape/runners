package auth_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	"github.com/lanescape/runners/emulators/oidc"
	"github.com/lanescape/runners/internal/auth"
)

const (
	orgID    = 303994381
	audience = "https://runners.example.test"
)

func delivery(body, sig string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/webhook", strings.NewReader(body))
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	return r
}

func TestReadSigned(t *testing.T) {
	secret := []byte("s3cret")
	body := `{"action":"queued"}`
	good := ghemu.Sign(secret, []byte(body))

	got, err := auth.ReadSigned(delivery(body, good), secret, 1024)
	if err != nil || string(got) != body {
		t.Fatalf("good delivery: %q, %v", got, err)
	}
	for _, tc := range []struct {
		name, body, sig string
		want            error
	}{
		{"unsigned", body, "", auth.ErrUnauthenticated},
		{"wrong secret", body, ghemu.Sign([]byte("other"), []byte(body)), auth.ErrUnauthenticated},
		{"changed body", body + " ", good, auth.ErrUnauthenticated},
		{"sha1 only", body, "sha1=0123", auth.ErrUnauthenticated},
		{"not hex", body, "sha256=zz", auth.ErrUnauthenticated},
		{"truncated signature", body, good[:20], auth.ErrUnauthenticated},
		{"too large", strings.Repeat("x", 1025), ghemu.Sign(secret, bytes.Repeat([]byte("x"), 1025)), auth.ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.ReadSigned(delivery(tc.body, tc.sig), secret, 1024); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := auth.ReadSigned(delivery(body, good), nil, 1024); err == nil {
		t.Fatal("a delivery verified with no secret configured")
	}
}

func TestBearer(t *testing.T) {
	for h, ok := range map[string]bool{"Bearer abc": true, "bearer abc": true, "": false, "Basic abc": false, "Bearer ": false, "Bearer a b": false} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Authorization", h)
		if _, err := auth.Bearer(r); (err == nil) != ok {
			t.Errorf("Bearer(%q) error = %v", h, err)
		}
	}
}

func TestGoogle(t *testing.T) {
	ctx := context.Background()
	iss := google.New()
	defer iss.Close()
	scheduler := "scheduler@lanescape-dev.iam.gserviceaccount.com"
	push := "pubsub-push@lanescape-dev.iam.gserviceaccount.com"
	v, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: iss.URL, JWKSURL: iss.JWKSURL}, Audience: audience}, []string{scheduler, push})
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{scheduler, push} {
		if got, err := v.Verify(ctx, iss.Token(acct, audience)); err != nil || got != acct {
			t.Fatalf("%s: %q, %v", acct, got, err)
		}
	}

	other := oidc.New()
	defer other.Close()
	with := func(change map[string]any) string {
		c := iss.Claims(scheduler, audience)
		for k, val := range change {
			c[k] = val
		}
		return iss.Mint(c)
	}
	for _, tc := range []struct {
		name  string
		token string
		want  error
	}{
		{"another account", iss.Token("someone@example.com", audience), auth.ErrForbidden},
		{"unverified email", with(map[string]any{"email_verified": false}), auth.ErrForbidden},
		{"no email_verified", with(map[string]any{"email_verified": nil}), auth.ErrForbidden},
		{"wrong audience", iss.Token(scheduler, "https://other.example.test"), auth.ErrUnauthenticated},
		{"expired", with(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), auth.ErrUnauthenticated},
		{"wrong issuer", with(map[string]any{"iss": "https://elsewhere.example.test"}), auth.ErrUnauthenticated},
		{"unpublished key", iss.MintWith(oidc.NewKey(), iss.Claims(scheduler, audience)), auth.ErrUnauthenticated},
		{"another issuer's key", other.Mint(iss.Claims(scheduler, audience)), auth.ErrUnauthenticated},
		{"garbage", "not.a.token", auth.ErrUnauthenticated},
		{"unsigned", unsigned(iss.Claims(scheduler, audience)), auth.ErrUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(ctx, tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	// A rotated key is fetched when first seen.
	iss.Rotate()
	if _, err := v.Verify(ctx, iss.Token(scheduler, audience)); err != nil {
		t.Fatalf("token signed with a rotated key: %v", err)
	}
}

// unsigned is a token with alg none.
func unsigned(claims map[string]any) string {
	full := oidc.Sign(oidc.NewKey(), claims)
	parts := strings.Split(full, ".")
	return "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + parts[1] + "."
}

func TestActions(t *testing.T) {
	ctx := context.Background()
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(orgID, "lanescape")
	repo := emu.Repo(org, 5001, "app")
	run := emu.PushRun(repo, "main")
	v, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: emu.Actions.URL, JWKSURL: emu.Actions.JWKSURL}, Audience: audience}, orgID)
	if err != nil {
		t.Fatal(err)
	}
	job := ghemu.Job{Audience: audience}
	c, err := v.Verify(ctx, emu.ActionsToken(run, job, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.RepositoryID != 5001 || c.RepositoryOwnerID != orgID || c.RunID != run.ID || c.RunAttempt != 1 ||
		c.EventName != "push" || c.Ref != "refs/heads/main" || c.RefType != "branch" || c.SHA != run.SHA ||
		c.Repository != "lanescape/app" || c.JobWorkflowRef != "lanescape/app/.github/workflows/ci.yml@refs/heads/main" {
		t.Fatalf("claims = %+v", c)
	}

	for _, tc := range []struct {
		name  string
		token string
		want  error
	}{
		{"another organization", emu.ActionsToken(run, job, map[string]any{"repository_owner_id": "1234"}), auth.ErrForbidden},
		{"look-alike name, other ID", emu.ActionsToken(run, job, map[string]any{"repository_owner": "lanescape", "repository_owner_id": "303994382"}), auth.ErrForbidden},
		{"no owner ID", emu.ActionsToken(run, job, map[string]any{"repository_owner_id": nil}), auth.ErrUnauthenticated},
		{"padded owner ID", emu.ActionsToken(run, job, map[string]any{"repository_owner_id": "0303994381"}), auth.ErrUnauthenticated},
		{"numeric owner ID", emu.ActionsToken(run, job, map[string]any{"repository_owner_id": orgID}), auth.ErrUnauthenticated},
		{"no run ID", emu.ActionsToken(run, job, map[string]any{"run_id": nil}), auth.ErrUnauthenticated},
		{"no repository ID", emu.ActionsToken(run, job, map[string]any{"repository_id": "x"}), auth.ErrUnauthenticated},
		{"no event", emu.ActionsToken(run, job, map[string]any{"event_name": nil}), auth.ErrUnauthenticated},
		{"default audience", emu.ActionsToken(run, ghemu.Job{}, nil), auth.ErrUnauthenticated},
		{"wrong audience", emu.ActionsToken(run, ghemu.Job{Audience: "https://other.example.test"}, nil), auth.ErrUnauthenticated},
		{"expired", emu.ActionsToken(run, job, map[string]any{"exp": time.Now().Add(-time.Second).Unix()}), auth.ErrUnauthenticated},
		{"wrong issuer", emu.ActionsToken(run, job, map[string]any{"iss": "https://token.actions.githubusercontent.com"}), auth.ErrUnauthenticated},
		{"unpublished key", emu.Actions.MintWith(oidc.NewKey(), emu.ActionsClaims(run, job)), auth.ErrUnauthenticated},
		{"unsigned", unsigned(emu.ActionsClaims(run, job)), auth.ErrUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(ctx, tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	// A Google token is no use here, even for the same audience.
	g := google.New()
	defer g.Close()
	if _, err := v.Verify(ctx, g.Token("scheduler@example.com", audience)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("Google token: %v", err)
	}
}

// A token whose issuer's keys can't be read isn't refused: it can't be
// checked yet, and the caller should try again.
func TestKeysThatCantBeRead(t *testing.T) {
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(orgID, "lanescape")
	run := emu.PushRun(emu.Repo(org, 5001, "app"), "main")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	v, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: emu.Actions.URL, JWKSURL: down.URL}, Audience: audience}, orgID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(context.Background(), emu.ActionsToken(run, ghemu.Job{Audience: audience}, nil))
	if !errors.Is(err, auth.ErrUnavailable) || errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("keys that can't be read: %v, want ErrUnavailable", err)
	}
}
