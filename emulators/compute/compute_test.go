package compute_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gce "google.golang.org/api/compute/v1"

	gceemu "github.com/lanescape/runners/emulators/compute"
)

// These tests check the emulator's metadata server and boot hooks, which
// the service's client doesn't reach.

const zone = "northamerica-northeast1-a"

func start(t *testing.T, enableGuest bool) (*gceemu.Emulator, *gce.Service) {
	t.Helper()
	emu := gceemu.New()
	t.Cleanup(emu.Close)
	emu.AddImage("base", "", "X86_64")
	svc, err := gce.NewService(context.Background(), emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	v := "TRUE"
	if !enableGuest {
		v = "FALSE"
	}
	inst := &gce.Instance{Name: "m", MachineType: "zones/" + zone + "/machineTypes/n4-highcpu-2",
		Metadata: &gce.Metadata{Items: []*gce.MetadataItems{{Key: "enable-guest-attributes", Value: &v}, {Key: "k", Value: new("value")}}},
		Disks: []*gce.AttachedDisk{{Boot: true, AutoDelete: true, InitializeParams: &gce.AttachedDiskInitializeParams{
			SourceImage: "global/images/base", DiskType: "zones/" + zone + "/diskTypes/hyperdisk-balanced"}}},
		ServiceAccounts: []*gce.ServiceAccount{{Email: "machine@lanescape-dev.iam.gserviceaccount.com"}}}
	if _, err := svc.Instances.Insert(emu.Project, zone, inst).Do(); err != nil {
		t.Fatal(err)
	}
	return emu, svc
}

func md(t *testing.T, method, url string, body string, flavor bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if flavor {
		req.Header.Set("Metadata-Flavor", "Google")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func claims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMetadataServer(t *testing.T) {
	emu, _ := start(t, true)
	base := emu.MetadataURL(zone, "m") + "/computeMetadata/v1/"
	if code, _ := md(t, "GET", base+"instance/attributes/k", "", false); code != http.StatusForbidden {
		t.Fatalf("without Metadata-Flavor: %d", code)
	}
	if code, v := md(t, "GET", base+"instance/attributes/k", "", true); code != 200 || v != "value" {
		t.Fatalf("attribute: %d %q", code, v)
	}
	if code, _ := md(t, "GET", base+"instance/attributes/missing", "", true); code != http.StatusNotFound {
		t.Fatalf("missing attribute: %d", code)
	}
	st, _ := emu.Instance(zone, "m")
	_, full := md(t, "GET", base+"instance/service-accounts/default/identity?audience=https://svc&format=full", "", true)
	c := claims(t, full)
	ce, _ := c["google"].(map[string]any)["compute_engine"].(map[string]any)
	if c["aud"] != "https://svc" || c["iss"] != emu.Identity.URL || c["email"] != "machine@lanescape-dev.iam.gserviceaccount.com" ||
		ce["instance_name"] != "m" || ce["zone"] != zone || ce["project_id"] != emu.Project || ce["instance_id"] != strconv.FormatUint(st.ID, 10) {
		t.Fatalf("full identity token's claims = %v", c)
	}
	_, std := md(t, "GET", base+"instance/service-accounts/default/identity?audience=https://svc", "", true)
	if _, ok := claims(t, std)["google"]; ok {
		t.Fatal("the standard format carries the instance's details")
	}
	if code, _ := md(t, "GET", base+"instance/service-accounts/default/identity", "", true); code != http.StatusBadRequest {
		t.Fatalf("no audience: %d", code)
	}
	if code, _ := md(t, "PUT", base+"instance/guest-attributes/lanescape-runners/state", "running", true); code != 200 {
		t.Fatalf("guest attribute: %d", code)
	}
	if code, v := md(t, "GET", base+"instance/guest-attributes/lanescape-runners/state", "", true); code != 200 || v != "running" {
		t.Fatalf("reading it back: %d %q", code, v)
	}
	if code, _ := md(t, "POST", emu.MetadataURL(zone, "m")+"/_emulator/poweroff", "", false); code != http.StatusNoContent {
		t.Fatalf("poweroff: %d", code)
	}
	if st, _ := emu.Instance(zone, "m"); st.Status != "TERMINATED" {
		t.Fatalf("after poweroff: %s", st.Status)
	}
	if code, _ := md(t, "GET", base+"instance/attributes/k", "", true); code != http.StatusServiceUnavailable {
		t.Fatalf("a stopped machine's metadata server: %d", code)
	}
}

func TestGuestAttributesNeedEnabling(t *testing.T) {
	emu, svc := start(t, false)
	base := emu.MetadataURL(zone, "m") + "/computeMetadata/v1/"
	if code, _ := md(t, "PUT", base+"instance/guest-attributes/ns/key", "v", true); code != http.StatusForbidden {
		t.Fatalf("disabled guest attributes: %d", code)
	}
	if _, err := svc.Instances.GetGuestAttributes(emu.Project, zone, "m").QueryPath("ns/").Do(); err == nil {
		t.Fatal("no guest attributes, but an answer")
	}
}

func TestBootAndStopHooks(t *testing.T) {
	emu := gceemu.New()
	defer emu.Close()
	emu.AddImage("base", "", "X86_64")
	booted, stopped := make(chan gceemu.Machine, 1), make(chan gceemu.Machine, 1)
	emu.OnBoot(func(m gceemu.Machine) { booted <- m })
	emu.OnStop(func(m gceemu.Machine) { stopped <- m })
	emu.SetOperationDelay(100 * time.Millisecond)
	svc, _ := gce.NewService(context.Background(), emu.ClientOptions()...)
	inst := &gce.Instance{Name: "m", MachineType: "zones/" + zone + "/machineTypes/n4-highcpu-2",
		Disks: []*gce.AttachedDisk{{Boot: true, InitializeParams: &gce.AttachedDiskInitializeParams{
			SourceImage: "global/images/base", DiskType: "zones/" + zone + "/diskTypes/hyperdisk-balanced"}}}}
	op, err := svc.Instances.Insert(emu.Project, zone, inst).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op.Status == "DONE" {
		t.Fatal("an operation with a delay was done at once")
	}
	if _, err := svc.ZoneOperations.Wait(emu.Project, zone, op.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Instances.Get(emu.Project, zone, "m").Do(); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-booted:
		if m.Name != "m" || m.MetadataURL == "" {
			t.Fatalf("booted %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never booted")
	}
	if _, err := svc.Instances.Delete(emu.Project, zone, "m").Do(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a deleted machine never stopped")
	}
}
