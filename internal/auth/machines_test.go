package auth_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	gce "google.golang.org/api/compute/v1"

	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/emulators/oidc"
	"github.com/lanescape/runners/internal/auth"
)

const machineAccount = "lanescape-runners-machine@lanescape-dev.iam.gserviceaccount.com"

func TestMachines(t *testing.T) {
	ctx := context.Background()
	emu := gceemu.New()
	defer emu.Close()
	emu.AddImage("base", "", "X86_64")
	svc, err := gce.NewService(ctx, emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	zone := emu.Zones()[0]
	for name, sa := range map[string]string{"m": machineAccount, "other": "other@lanescape-dev.iam.gserviceaccount.com"} {
		inst := &gce.Instance{Name: name, MachineType: "zones/" + zone + "/machineTypes/n4-highcpu-2",
			Disks: []*gce.AttachedDisk{{Boot: true, InitializeParams: &gce.AttachedDiskInitializeParams{
				SourceImage: "global/images/base", DiskType: "zones/" + zone + "/diskTypes/hyperdisk-balanced"}}},
			ServiceAccounts: []*gce.ServiceAccount{{Email: sa}}}
		if _, err := svc.Instances.Insert(emu.Project, zone, inst).Do(); err != nil {
			t.Fatal(err)
		}
	}
	v, err := auth.NewMachines(auth.Options{Issuer: auth.Issuer{URL: emu.Identity.URL, JWKSURL: emu.Identity.JWKSURL}, Audience: audience},
		emu.Project, machineAccount)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := emu.Instance(zone, "m")
	c, err := v.Verify(ctx, emu.Identity.Mint(emu.IdentityClaims(zone, "m", audience, true)))
	if err != nil {
		t.Fatal(err)
	}
	if c.InstanceID != st.ID || c.InstanceName != "m" || c.Zone != zone || c.ProjectID != emu.Project || c.Email != machineAccount {
		t.Fatalf("claims = %+v", c)
	}
	full := func(change func(map[string]any)) string {
		cl := emu.IdentityClaims(zone, "m", audience, true)
		change(cl)
		return emu.Identity.Mint(cl)
	}
	ce := func(cl map[string]any) map[string]any {
		return cl["google"].(map[string]any)["compute_engine"].(map[string]any)
	}
	for _, tc := range []struct {
		name  string
		token string
		want  error
	}{
		{"standard format", emu.Identity.Mint(emu.IdentityClaims(zone, "m", audience, false)), auth.ErrUnauthenticated},
		{"another audience", emu.Identity.Mint(emu.IdentityClaims(zone, "m", "https://other.example.test", true)), auth.ErrUnauthenticated},
		{"another account", emu.Identity.Mint(emu.IdentityClaims(zone, "other", audience, true)), auth.ErrForbidden},
		{"unverified email", full(func(cl map[string]any) { cl["email_verified"] = false }), auth.ErrForbidden},
		{"another project", full(func(cl map[string]any) { ce(cl)["project_id"] = "elsewhere" }), auth.ErrForbidden},
		{"no instance", full(func(cl map[string]any) { delete(ce(cl), "instance_id") }), auth.ErrUnauthenticated},
		{"instance ID not a number", full(func(cl map[string]any) { ce(cl)["instance_id"] = "x" + strconv.FormatUint(st.ID, 10) }), auth.ErrUnauthenticated},
		{"expired", full(func(cl map[string]any) { cl["exp"] = 1 }), auth.ErrUnauthenticated},
		{"signed by an unpublished key", emu.Identity.MintWith(oidc.NewKey(), emu.IdentityClaims(zone, "m", audience, true)), auth.ErrUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(ctx, tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
