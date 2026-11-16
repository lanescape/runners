package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/service"
)

// The token comes from the machine's metadata server, as this repository's
// emulator of the service serves its machines one, for the audience asked
// for, in the full format.
func TestToken(t *testing.T) {
	svc := service.New()
	defer svc.Close()
	repo := svc.Repository("app")
	j, err := svc.StartJob(svc.Push(repo, "main"), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Server{URL: svc.Metadata(j)}.Token(context.Background(), svc.URL())
	if err != nil || tok == "" || tok != svc.Token(j) && len(tok) < 100 {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if _, err := (Server{URL: svc.Metadata(j)}).Token(context.Background(), ""); err == nil {
		t.Fatal("a token with no audience")
	}
	if _, err := (Server{URL: "http://127.0.0.1:1"}).Token(context.Background(), "aud"); err == nil {
		t.Fatal("no error from a server that isn't there")
	}
}

// On one of the service's machines the metadata server answers the
// attribute lsr-service; anywhere else there's no metadata server, or no
// attribute, and the action decides at once.
func TestOnService(t *testing.T) {
	svc := service.New()
	defer svc.Close()
	j, err := svc.StartJob(svc.Push(svc.Repository("app"), "main"), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !(Server{URL: svc.Metadata(j)}).OnService(context.Background()) {
		t.Fatal("the service's machine isn't one")
	}
	hang := make(chan struct{})
	defer close(hang)
	for name, h := range map[string]http.HandlerFunc{
		"no attribute": func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"an empty one": func(w http.ResponseWriter, _ *http.Request) {},
		"a redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, svc.Metadata(j)+"/computeMetadata/v1/instance/attributes/lsr-service", http.StatusFound)
		},
		"no answer": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			start := time.Now()
			if (Server{URL: srv.URL, AskFor: 200 * time.Millisecond}).OnService(context.Background()) {
				t.Fatal("a machine that isn't the service's is one")
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("took %s to decide", d)
			}
		})
	}
	if (Server{URL: "http://127.0.0.1:1"}).OnService(context.Background()) {
		t.Fatal("no metadata server, and it's the service's machine")
	}
	if AttributeTimeout < time.Second || AttributeTimeout > 5*time.Second {
		t.Fatalf("it asks for %s, not a few seconds", AttributeTimeout)
	}
}
