package compute_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/internal/compute"
)

const (
	zoneA   = "northamerica-northeast1-a"
	zoneB   = "northamerica-northeast1-b"
	account = "lanescape-runners-machine@lanescape-dev.iam.gserviceaccount.com"
)

func setup(t *testing.T, opts ...gceemu.Option) (*gceemu.Emulator, *compute.Client) {
	t.Helper()
	emu := gceemu.New(opts...)
	t.Cleanup(emu.Close)
	emu.AddImage("lsr-base-amd64-1", "", "X86_64")
	emu.AddImage("lsr-base-arm64-1", "", "ARM64")
	c, err := compute.New(context.Background(), emu.Project, emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	return emu, c
}

func spec(name, machineType string, spot bool) compute.InstanceSpec {
	return compute.InstanceSpec{
		Name: name, MachineType: machineType, Spot: spot, Image: "lsr-base-amd64-1", BootDiskGB: 100,
		DiskType: "hyperdisk-balanced", ProvisionedIOPS: 3000, ProvisionedThroughput: 400,
		Labels:         map[string]string{"lsr-repository": "5001"},
		Metadata:       map[string]string{"lsr-selector": "lanescape-2vcpu-linux-amd64-main"},
		ServiceAccount: account, Scopes: []string{"https://www.googleapis.com/auth/userinfo.email"},
		Network: "lanescape-runners", MaxRunDuration: 6 * time.Hour,
	}
}

func TestInstances(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("a")); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-bbbbbbbb", "n4-highcpu-2", true), compute.RequestID("b")); err != nil {
		t.Fatal(err)
	}
	ins, err := c.Instances(ctx, zoneA)
	if err != nil || len(ins) != 2 {
		t.Fatalf("instances = %+v, %v", ins, err)
	}
	a, b := ins[0], ins[1]
	if a.Name != "lsr-5001-aaaaaaaa" || a.ID == 0 || a.Status != "RUNNING" || a.Spot || a.MachineType != "n4-highcpu-2" ||
		a.Labels["lsr-repository"] != "5001" || a.Metadata["lsr-selector"] != "lanescape-2vcpu-linux-amd64-main" ||
		a.Zone != zoneA || a.Created.IsZero() || len(a.Disks) != 1 || !a.Disks[0].Boot || a.Disks[0].Disk != a.Name {
		t.Fatalf("standard instance = %+v", a)
	}
	if !b.Spot {
		t.Fatalf("spot instance = %+v", b)
	}
	st, _ := emu.Instance(zoneA, b.Name)
	if st.Provisioning != "SPOT" || st.Termination != "STOP" || st.MaxRun != 6*time.Hour || st.ServiceAccount != account ||
		st.Network != "lanescape-runners" || !st.ExternalIP {
		t.Fatalf("emulated spot instance = %+v", st)
	}
	ds := emu.Disks()
	if len(ds) != 2 || ds[0].SizeGB != 100 || ds[0].IOPS != 3000 || ds[0].Throughput != 400 || ds[0].Type != "hyperdisk-balanced" {
		t.Fatalf("boot disks = %+v", ds)
	}
	if _, err := c.Instance(ctx, zoneB, a.Name); !errors.Is(err, compute.ErrNotFound) {
		t.Fatalf("in the other zone: %v", err)
	}
	if err := c.StopInstance(ctx, zoneA, a.Name, compute.RequestID("stop", a.Name)); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Instance(ctx, zoneA, a.Name); got.Status != "TERMINATED" || !got.Stopped() {
		t.Fatalf("stopped: %+v", got)
	}
	if err := c.DeleteInstance(ctx, zoneA, a.Name, compute.RequestID("delete", a.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Instance(ctx, zoneA, a.Name); !errors.Is(err, compute.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if len(emu.Disks()) != 1 {
		t.Fatal("a deleted instance's boot disk wasn't deleted with it")
	}
}

func TestCapacity(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	emu.Stockout(zoneA, "n4-highcpu-2", true)
	err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", true), compute.RequestID("1"))
	var ce *compute.CapacityError
	if !errors.As(err, &ce) || !ce.Stockout {
		t.Fatalf("stockout: %v", err)
	}
	// Only Spot is short.
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("2")); err != nil {
		t.Fatalf("standard: %v", err)
	}
	emu.SetQuota("northamerica-northeast1", "N4_CPUS", 3)
	err = c.CreateInstance(ctx, zoneB, spec("lsr-5001-bbbbbbbb", "n4-highcpu-2", false), compute.RequestID("3"))
	if !errors.As(err, &ce) || ce.Stockout {
		t.Fatalf("quota: %v", err)
	}
	// C4's quota is apart.
	if err := c.CreateInstance(ctx, zoneB, spec("lsr-5001-bbbbbbbb", "c4-highcpu-2", false), compute.RequestID("4")); err != nil {
		t.Fatalf("another family: %v", err)
	}
}

func TestRequestIDs(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	s := spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false)
	// The first answer is lost after the work is done; the retry, with the
	// same ID, gets the same operation.
	emu.LoseResponse(http.MethodPost, "/instances", 1)
	if err := c.CreateInstance(ctx, zoneA, s, compute.RequestID("x")); err != nil {
		t.Fatalf("after a lost response: %v", err)
	}
	if err := c.CreateInstance(ctx, zoneA, s, compute.RequestID("x")); err != nil {
		t.Fatalf("the same request again: %v", err)
	}
	if n := len(emu.Instances()); n != 1 {
		t.Fatalf("%d instances, want 1", n)
	}
	if err := c.CreateInstance(ctx, zoneA, s, compute.RequestID("y")); !errors.Is(err, compute.ErrExists) {
		t.Fatalf("another request for the same name: %v, want ErrExists", err)
	}
	// A request that never arrived is sent again.
	emu.ServerError(http.MethodPost, "/stop", http.StatusServiceUnavailable, 1)
	if err := c.StopInstance(ctx, zoneA, s.Name, compute.RequestID("stop")); err != nil {
		t.Fatal(err)
	}
	if id := compute.RequestID("a", "b"); id != compute.RequestID("a", "b") || id == compute.RequestID("ab") || len(id) != 36 || id[14] != '4' {
		t.Fatalf("request IDs: %s", id)
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	_, c := setup(t)
	arm := spec("lsr-5001-aaaaaaaa", "c4a-highcpu-16", false)
	if err := c.CreateInstance(ctx, zoneA, arm, compute.RequestID("1")); err == nil || compute.IsUnavailable(err) {
		t.Fatalf("an amd64 image on an Arm machine: %v, want a confirmed refusal", err)
	}
	pd := spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false)
	pd.DiskType = "pd-balanced"
	var f *compute.Failed
	if err := c.CreateInstance(ctx, zoneA, pd, compute.RequestID("2")); !errors.As(err, &f) {
		t.Fatalf("a persistent disk on N4: %v, want Failed", err)
	}
}

func TestDisks(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("i")); err != nil {
		t.Fatal(err)
	}
	ds := compute.DiskSpec{Name: "lsr-5001-aaaaaaaa-work", SizeGB: 50, DiskType: "hyperdisk-balanced", ProvisionedIOPS: 3000,
		ProvisionedThroughput: 400, Labels: map[string]string{"lsr-repository": "5001"}}
	if err := c.CreateDisk(ctx, zoneA, ds, compute.RequestID("d")); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateDisk(ctx, zoneA, ds, compute.RequestID("d")); err != nil {
		t.Fatalf("the same request again: %v", err)
	}
	if err := c.AttachDisk(ctx, zoneA, "lsr-5001-aaaaaaaa", ds.Name, "work", compute.RequestID("att")); err != nil {
		t.Fatal(err)
	}
	d, err := c.Disk(ctx, zoneA, ds.Name)
	if err != nil || !slices.Equal(d.Users, []string{"lsr-5001-aaaaaaaa"}) || d.SizeGB != 50 {
		t.Fatalf("disk = %+v, %v", d, err)
	}
	in, _ := c.Instance(ctx, zoneA, "lsr-5001-aaaaaaaa")
	if len(in.Disks) != 2 || in.Disks[1].DeviceName != "work" || in.Disks[1].Disk != ds.Name || !in.Disks[1].AutoDelete {
		t.Fatalf("instance's disks = %+v", in.Disks)
	}
	if err := c.DeleteDisk(ctx, zoneA, ds.Name, compute.RequestID("del")); !errors.Is(err, compute.ErrInUse) {
		t.Fatalf("deleting an attached disk: %v, want ErrInUse", err)
	}
	if err := c.DeleteInstance(ctx, zoneA, "lsr-5001-aaaaaaaa", compute.RequestID("di")); err != nil {
		t.Fatal(err)
	}
	if n := len(emu.Disks()); n != 0 {
		t.Fatalf("%d disks left after deleting their instance", n)
	}
	emu.AddDisk(zoneB, "lsr-5001-cccccccc-work", 50, nil)
	all, err := c.Disks(ctx, zoneB)
	if err != nil || len(all) != 1 || len(all[0].Users) != 0 {
		t.Fatalf("disks = %+v, %v", all, err)
	}
	if err := c.DeleteDisk(ctx, zoneB, "lsr-5001-cccccccc-work", compute.RequestID("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDisk(ctx, zoneB, "lsr-5001-cccccccc-work", compute.RequestID("y")); !errors.Is(err, compute.ErrNotFound) {
		t.Fatalf("deleting it again: %v", err)
	}
}

func TestPreemption(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", true), compute.RequestID("1")); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-bbbbbbbb", "n4-highcpu-2", false), compute.RequestID("2")); err != nil {
		t.Fatal(err)
	}
	if err := emu.Preempt(zoneA, "lsr-5001-bbbbbbbb"); err == nil {
		t.Fatal("a standard machine was preempted")
	}
	spot, _ := c.Instance(ctx, zoneA, "lsr-5001-aaaaaaaa")
	if err := emu.Preempt(zoneA, spot.Name); err != nil {
		t.Fatal(err)
	}
	ids, err := c.Preempted(ctx, zoneA)
	if err != nil || !slices.Equal(ids, []uint64{spot.ID}) {
		t.Fatalf("preempted = %v, %v; want %d", ids, err, spot.ID)
	}
	if got, _ := c.Instance(ctx, zoneA, spot.Name); got.Status != "TERMINATED" {
		t.Fatalf("preempted machine is %s", got.Status)
	}
	emu.ForgetOperations()
	if ids, _ := c.Preempted(ctx, zoneA); len(ids) != 0 {
		t.Fatal("forgotten operations still listed")
	}
}

func TestMaxRunDuration(t *testing.T) {
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Now())
	emu, c := setup(t, gceemu.WithClock(clock))
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("1")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(6*time.Hour + time.Second)
	if got, _ := c.Instance(ctx, zoneA, "lsr-5001-aaaaaaaa"); got.Status != "TERMINATED" {
		t.Fatalf("past its maximum run duration: %s", got.Status)
	}
	_ = emu
}

func TestSerialOutput(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("1")); err != nil {
		t.Fatal(err)
	}
	emu.WriteSerial(zoneA, "lsr-5001-aaaaaaaa", "booting\nsupervisor: started\n")
	out, err := c.SerialOutput(ctx, zoneA, "lsr-5001-aaaaaaaa", 1<<20)
	if err != nil || out != "booting\nsupervisor: started\n" {
		t.Fatalf("serial = %q, %v", out, err)
	}
	if out, _ := c.SerialOutput(ctx, zoneA, "lsr-5001-aaaaaaaa", 7); out != "booting" {
		t.Fatalf("limited serial = %q", out)
	}
}

func TestImages(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	ims, err := c.Images(ctx)
	if err != nil || len(ims) != 2 || ims[0].Name != "lsr-base-amd64-1" || ims[0].Status != "READY" || ims[0].Architecture != "X86_64" {
		t.Fatalf("images = %+v, %v", ims, err)
	}
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("i")); err != nil {
		t.Fatal(err)
	}
	ds := compute.DiskSpec{Name: "lsr-5001-aaaaaaaa-work", SizeGB: 50, DiskType: "hyperdisk-balanced", ProvisionedIOPS: 3000, ProvisionedThroughput: 400}
	if err := c.CreateDisk(ctx, zoneA, ds, compute.RequestID("d")); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachDisk(ctx, zoneA, "lsr-5001-aaaaaaaa", ds.Name, "work", compute.RequestID("att")); err != nil {
		t.Fatal(err)
	}
	is := compute.ImageSpec{Name: "lsr-5001-0123456789abcdef0123-1", SourceZone: zoneA, SourceDisk: ds.Name, Labels: map[string]string{"lsr-repository": "5001"}}
	if err := c.CreateImage(ctx, is, compute.RequestID("im")); err == nil || compute.IsUnavailable(err) {
		t.Fatalf("an image of a running machine's disk: %v, want a confirmed refusal", err)
	}
	if err := c.StopInstance(ctx, zoneA, "lsr-5001-aaaaaaaa", compute.RequestID("stop")); err != nil {
		t.Fatal(err)
	}
	// Its first answer is lost after the work is done; the retry with the
	// same ID gets the same operation.
	emu.LoseResponse(http.MethodPost, "/global/images", 1)
	if err := c.CreateImage(ctx, is, compute.RequestID("im2")); err != nil {
		t.Fatal(err)
	}
	im, err := c.Image(ctx, is.Name)
	if err != nil || im.Status != "READY" || im.DiskSizeGB != 50 || im.SourceDisk != ds.Name || im.Labels["lsr-repository"] != "5001" || im.LabelFingerprint == "" {
		t.Fatalf("image = %+v, %v", im, err)
	}
	if err := c.SetImageLabels(ctx, is.Name, map[string]string{"lsr-repository": "5001", "lsr-last-used": "2026-11-08"}, im.LabelFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := c.SetImageLabels(ctx, is.Name, map[string]string{}, im.LabelFingerprint); !errors.Is(err, compute.ErrFingerprint) {
		t.Fatalf("labels with a stale fingerprint: %v, want ErrFingerprint", err)
	}
	if got, _ := c.Image(ctx, is.Name); got.Labels["lsr-last-used"] != "2026-11-08" {
		t.Fatalf("labels = %v", got.Labels)
	}
	emu.FailNextOperation("images.insert", "INTERNAL_ERROR")
	var f *compute.Failed
	if err := c.CreateImage(ctx, compute.ImageSpec{Name: "lsr-5001-0123456789abcdef0123-2", SourceZone: zoneA, SourceDisk: ds.Name},
		compute.RequestID("im3")); !errors.As(err, &f) {
		t.Fatalf("a failed operation: %v, want Failed", err)
	}
	if err := c.DeleteImage(ctx, is.Name, compute.RequestID("del")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Image(ctx, is.Name); !errors.Is(err, compute.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if err := c.DeleteImage(ctx, is.Name, compute.RequestID("del2")); !errors.Is(err, compute.ErrNotFound) {
		t.Fatalf("deleting it again: %v", err)
	}
}

func TestSubnetwork(t *testing.T) {
	emu, c := setup(t)
	s := spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false)
	s.Subnetwork = "lanescape-runners"
	if err := c.CreateInstance(context.Background(), zoneB, s, compute.RequestID("1")); err != nil {
		t.Fatal(err)
	}
	if st, _ := emu.Instance(zoneB, s.Name); st.Network != "lanescape-runners" || st.Subnetwork != "lanescape-runners" {
		t.Fatalf("instance = %+v", st)
	}
	if compute.Region(zoneB) != "northamerica-northeast1" {
		t.Fatalf("region = %s", compute.Region(zoneB))
	}
}

func TestOperationsAreWaitedFor(t *testing.T) {
	ctx := context.Background()
	emu, c := setup(t)
	emu.SetOperationDelay(300 * time.Millisecond)
	start := time.Now()
	if err := c.CreateInstance(ctx, zoneA, spec("lsr-5001-aaaaaaaa", "n4-highcpu-2", false), compute.RequestID("1")); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("returned before the operation finished")
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.StopInstance(short, zoneA, "lsr-5001-aaaaaaaa", compute.RequestID("2")); !compute.IsUnavailable(err) {
		t.Fatalf("an operation that outlives its context: %v, want Unavailable", err)
	}
}
