package oidc_test

import (
	"context"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/lanescape/runners/emulators/oidc"
)

func TestDiscoveryAndKeys(t *testing.T) {
	ctx := context.Background()
	iss := oidc.New()
	defer iss.Close()
	p, err := gooidc.NewProvider(ctx, iss.URL)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	v := p.Verifier(&gooidc.Config{ClientID: "aud"})
	if _, err := v.Verify(ctx, iss.Mint(map[string]any{"aud": "aud", "sub": "x"})); err != nil {
		t.Fatalf("minted token: %v", err)
	}
	if _, err := v.Verify(ctx, iss.MintWith(oidc.NewKey(), map[string]any{"aud": "aud"})); err == nil {
		t.Fatal("a token signed with an unpublished key verified")
	}
	iss.Rotate()
	if _, err := v.Verify(ctx, iss.Mint(map[string]any{"aud": "aud"})); err != nil {
		t.Fatalf("token after rotation: %v", err)
	}
	if _, err := v.Verify(ctx, iss.Mint(map[string]any{"aud": "aud", "exp": nil})); err == nil {
		t.Fatal("a token without exp verified")
	}
}
