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

// CPUS_ALL_REGIONS counts every machine in every region, and an operation
// that reports it names it as Compute Engine does.
func TestAllRegionsQuota(t *testing.T) {
	ctx := context.Background()
	emu := gceemu.New(gceemu.WithZones("northamerica-northeast1-a", "us-central1-a"))
	t.Cleanup(emu.Close)
	emu.AddImage("base", "", "X86_64")
	emu.SetGlobalQuota("CPUS_ALL_REGIONS", 4)
	emu.ReportQuotaInOperation(true)
	svc, err := gce.NewService(ctx, emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(z, name, mt string) *gce.Operation {
		inst := &gce.Instance{Name: name, MachineType: "zones/" + z + "/machineTypes/" + mt,
			Disks: []*gce.AttachedDisk{{Boot: true, AutoDelete: true, InitializeParams: &gce.AttachedDiskInitializeParams{
				SourceImage: "global/images/base", DiskType: "zones/" + z + "/diskTypes/hyperdisk-balanced"}}}}
		op, err := svc.Instances.Insert(emu.Project, z, inst).Do()
		if err != nil {
			t.Fatal(err)
		}
		for op.Status != "DONE" {
			if op, err = svc.ZoneOperations.Wait(emu.Project, z, op.Name).Do(); err != nil {
				t.Fatal(err)
			}
		}
		return op
	}
	if op := insert("northamerica-northeast1-a", "a", "n4-highcpu-2"); op.Error != nil {
		t.Fatalf("within the quota: %+v", op.Error.Errors[0])
	}
	// Another region, another family: the same quota.
	op := insert("us-central1-a", "b", "c4-highcpu-4")
	if op.Error == nil || len(op.Error.Errors) != 1 {
		t.Fatal("a machine past the quota in another region was made")
	}
	e := op.Error.Errors[0]
	if e.Code != "QUOTA_EXCEEDED" || len(e.ErrorDetails) != 1 || e.ErrorDetails[0].QuotaInfo == nil {
		t.Fatalf("error %+v", e)
	}
	if q := e.ErrorDetails[0].QuotaInfo; q.MetricName != "compute.googleapis.com/cpus_all_regions" || q.LimitName != "CPUS-ALL-REGIONS-per-project" || q.Limit != 4 {
		t.Fatalf("quota info %+v", q)
	}
	if op := insert("us-central1-a", "c", "c4-highcpu-2"); op.Error != nil {
		t.Fatalf("within the quota, in another region: %+v", op.Error.Errors[0])
	}
}

// Hyperdisk Balanced's limits, at and either side of each boundary.
func TestHyperdiskBalancedLimits(t *testing.T) {
	for _, tc := range []struct {
		size, iops, throughput int64
		ok                     bool
	}{
		{4, 2000, 140, true}, {4, 2001, 140, false}, {4, 1999, 140, false},
		{5, 2500, 140, true}, {6, 3000, 140, true}, {6, 2999, 140, false},
		{50, 3000, 400, true}, {50, 25000, 0, true}, {50, 25001, 0, false},
		{500, 160000, 2400, true}, {500, 160001, 2400, false},
		{500, 3001, 750, true}, {500, 3001, 751, false},
		{500, 40000, 156, true}, {500, 40000, 155, false},
		{500, 10000, 2400, true}, {500, 10000, 2501, false},
		{3, 0, 0, false}, {65536, 0, 0, true}, {65537, 0, 0, false},
		{50, 0, 825, true}, {50, 0, 826, false},
		{4, 0, 500, true}, {4, 0, 600, false}, {500, 0, 1000, true}, {500, 0, 1501, false},
	} {
		why := gceemu.HyperdiskBalancedRefusal(tc.size, tc.iops, tc.throughput)
		if (why == "") != tc.ok {
			t.Errorf("%d GB, %d IOPS, %d MiB/s: refusal %q; want allowed %v", tc.size, tc.iops, tc.throughput, why, tc.ok)
		}
	}
}
