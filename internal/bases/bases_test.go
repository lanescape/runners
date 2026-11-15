package bases_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	gceemu "github.com/lanescape/runners/emulators/compute"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/bases"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
	"github.com/lanescape/runners/machine"
)

const (
	zoneA = "northamerica-northeast1-a"
	zoneB = "northamerica-northeast1-b"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type env struct {
	t       *testing.T
	clock   *clockwork.FakeClock
	gce     *gceemu.Emulator
	gcec    *compute.Client
	objects store.Objects
	cfg     *config.Config
	svc     *bases.Service
	runner  *pass.Runner
	logs    *lockedBuffer
	keys    map[string]string
	// short is what the last pass held from jobs.
	short []string
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	// The storage emulator stamps records with the real time, which a
	// record's age is read from.
	clock := clockwork.NewFakeClockAt(time.Now())
	gce := gceemu.New(gceemu.WithClock(clock))
	t.Cleanup(gce.Close)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	gce.AddPublicImage(config.DebianProject, cfg.Base.Debian["amd64"], "X86_64")
	gce.AddPublicImage(config.DebianProject, cfg.Base.Debian["arm64"], "ARM64")
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")
	objects, err := store.NewGCS(ctx, "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	gcec, err := compute.New(ctx, gce.Project, gce.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	svc := &bases.Service{Config: cfg, Compute: gcec, Objects: objects, Clock: clock, Logger: log,
		Network: "lanescape-runners", Subnetwork: "lanescape-runners", DiskType: "hyperdisk-balanced"}
	return &env{t: t, clock: clock, gce: gce, gcec: gcec, objects: objects, cfg: cfg, svc: svc, logs: logs, keys: bases.Keys(cfg),
		runner: &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}}
}

func (e *env) pass() {
	e.t.Helper()
	if err := e.runner.Run(context.Background(), func(p *pass.Pass) error { e.short = e.svc.Pass(p); return nil }); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *env) builder(arch string) (gceemu.InstanceState, bool) {
	name := bases.BuilderName(arch, e.keys[arch])
	for _, in := range e.gce.Instances() {
		if in.Name == name {
			return in, true
		}
	}
	return gceemu.InstanceState{}, false
}

func (e *env) builders() []gceemu.InstanceState {
	var out []gceemu.InstanceState
	for _, in := range e.gce.Instances() {
		if strings.HasPrefix(in.Name, bases.BuilderPrefix) {
			out = append(out, in)
		}
	}
	return out
}

func (e *env) image(name string) (gceemu.ImageState, bool) {
	for _, im := range e.gce.Images() {
		if im.Name == name {
			return im, true
		}
	}
	return gceemu.ImageState{}, false
}

func (e *env) record(arch string) (bases.Record, bool) {
	e.t.Helper()
	data, _, err := e.objects.Get(context.Background(), "bases/"+arch+"-"+e.keys[arch]+".json")
	if errors.Is(err, store.ErrNotFound) {
		return bases.Record{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var r bases.Record
	if err := json.Unmarshal(data, &r); err != nil {
		e.t.Fatal(err)
	}
	return r, true
}

func (e *env) boot(arch string) string {
	e.t.Helper()
	ims, err := e.gcec.Images(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	name, _ := e.svc.Boot(ims, arch)
	return name
}

// programs is the lsr-programs label of the images this service makes.
func programs() string { return bases.ProgramsLabel(machine.Digest()) }

// labelled are an image's labels as one made with programs, for its
// architecture and key.
func labelled(name, programs string) map[string]string {
	rest := strings.TrimPrefix(name, bases.ImagePrefix)
	arch, key, _ := strings.Cut(rest, "-")
	l := map[string]string{bases.LabelKind: bases.KindBase, bases.LabelArch: arch, bases.LabelKey: key}
	if programs != "" {
		l[bases.LabelPrograms] = programs
	}
	return l
}

func gceArch(arch string) string { return map[string]string{"amd64": "X86_64", "arm64": "ARM64"}[arch] }

// say writes a builder's result to its console, as the script does.
func (e *env) say(b gceemu.InstanceState, line string) {
	e.gce.WriteSerial(b.Zone, b.Name, "[   12.000] some boot message\nstartup-script: installing\n\n"+line+"\n")
}

func (e *env) built(arch string) string {
	return machine.Built + arch + "-" + e.keys[arch]
}

func TestKey(t *testing.T) {
	in := bases.Inputs{Arch: "amd64", Debian: "debian-13-trixie-v20261109", RunnerVersion: "2.338.2", RunnerSHA256: strings.Repeat("a", 64),
		GoVersion: "1.27.3", GoSHA256: strings.Repeat("b", 64), Script: "#!/bin/bash\n", Programs: strings.Repeat("c", 64)}
	k := bases.Key(in)
	if len(k) != 20 || strings.Trim(k, "0123456789abcdef") != "" {
		t.Fatalf("key %q isn't 20 hex digits", k)
	}
	if k != bases.Key(in) {
		t.Fatal("the key isn't the same for the same inputs")
	}
	// Each input changes the key, and moving bytes between two inputs does
	// too.
	for name, change := range map[string]func(*bases.Inputs){
		"arch":           func(i *bases.Inputs) { i.Arch = "arm64" },
		"debian":         func(i *bases.Inputs) { i.Debian = "debian-13-trixie-v20261201" },
		"runner version": func(i *bases.Inputs) { i.RunnerVersion = "2.339.0" },
		"runner sha256":  func(i *bases.Inputs) { i.RunnerSHA256 = strings.Repeat("d", 64) },
		"go version":     func(i *bases.Inputs) { i.GoVersion = "1.27.4" },
		"go sha256":      func(i *bases.Inputs) { i.GoSHA256 = strings.Repeat("e", 64) },
		"script":         func(i *bases.Inputs) { i.Script = "#!/bin/bash\nset -e\n" },
		"programs":       func(i *bases.Inputs) { i.Programs = strings.Repeat("f", 64) },
		"boundary":       func(i *bases.Inputs) { i.GoVersion, i.GoSHA256 = i.GoVersion+"b", strings.Repeat("b", 63) },
	} {
		c := in
		change(&c)
		if bases.Key(c) == k {
			t.Errorf("changing the %s doesn't change the key", name)
		}
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	keys := bases.Keys(cfg)
	if keys["amd64"] == keys["arm64"] || len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
	// The real inputs are the configuration's and what's embedded.
	got := bases.InputsOf(cfg, "arm64")
	if got.Debian != cfg.Base.Debian["arm64"] || got.Script != machine.BuildScript() || got.Programs != machine.Digest() ||
		got.GoSHA256 != cfg.Base.Go.SHA256["arm64"] || got.RunnerSHA256 != cfg.Runner.SHA256["arm64"] {
		t.Fatalf("inputs = %+v", got)
	}
	if n := bases.ImageName("amd64", keys["amd64"]); len(n) > 63 || n != "lsr-base-amd64-"+keys["amd64"] {
		t.Errorf("image name %s", n)
	}
	if n := bases.BuilderName("arm64", keys["arm64"]); len(n) > 63 || n != "lsr-build-arm64-"+keys["arm64"] {
		t.Errorf("builder name %s", n)
	}
}

func TestResult(t *testing.T) {
	key := "0123456789abcdef0123"
	for _, tc := range []struct {
		console string
		built   bool
		failed  string
	}{
		{"", false, ""},
		{"booting\n" + machine.Built + "amd64-" + key + "\n", true, ""},
		{"booting\n" + machine.Built + "amd64-" + key + "\r\n", true, ""},
		{"booting\nstartup-script: " + machine.Built + "amd64-" + key + "\n", true, ""},
		{machine.Built + "arm64-" + key + "\n", false, ""},
		{machine.Built + "amd64-" + key + "0\n", false, ""},
		{machine.Built + "amd64-" + key[:19] + "\n", false, ""},
		{"say \"building amd64-" + key + "\"\n", false, ""},
		{machine.Failed + "line 40 exited 1: curl ...\n", false, "line 40 exited 1: curl ..."},
		{machine.Failed + "\n", false, "no reason given"},
		{machine.Failed + "late\n" + machine.Built + "amd64-" + key + "\n", true, "late"},
		{machine.Failed + "bell\x07 " + strings.Repeat("x", 600) + "\n", false, "bell " + strings.Repeat("x", 495) + "..."},
	} {
		built, failed := bases.Result(tc.console, "amd64", key)
		if built != tc.built || failed != tc.failed {
			t.Errorf("Result(%q) = %v, %q; want %v, %q", tc.console, built, failed, tc.built, tc.failed)
		}
	}
}

func TestBoot(t *testing.T) {
	t0 := time.Now()
	const progs = "0123456789abcdef0123"
	same := map[string]string{bases.LabelPrograms: progs}
	other := map[string]string{bases.LabelPrograms: "ffffffffffffffffffff"}
	ims := []compute.Image{
		{Name: "lsr-base-amd64-aaaa", Status: "READY", Created: t0},
		{Name: "lsr-base-amd64-bbbb", Status: "READY", Created: t0.Add(time.Hour), Labels: same},
		{Name: "lsr-base-amd64-cccc", Status: "PENDING", Created: t0.Add(2 * time.Hour), Labels: same},
		{Name: "lsr-base-amd64-dddd", Status: "FAILED", Created: t0.Add(3 * time.Hour), Labels: same},
		{Name: "lsr-base-arm64-eeee", Status: "READY", Created: t0.Add(4 * time.Hour), Labels: same},
		{Name: "lsr-5001-0123456789abcdef0123-1-abcdefgh", Status: "READY", Created: t0.Add(5 * time.Hour), Labels: same},
		// Newer than bbbb, but made with other programs, or with no record
		// of them, as images made before they had the label.
		{Name: "lsr-base-amd64-gggg", Status: "READY", Created: t0.Add(6 * time.Hour), Labels: other},
		{Name: "lsr-base-amd64-hhhh", Status: "READY", Created: t0.Add(7 * time.Hour)},
		{Name: "lsr-base-arm64-iiii", Status: "READY", Created: t0.Add(8 * time.Hour), Labels: other},
	}
	for _, tc := range []struct {
		arch, key, want string
	}{
		{"amd64", "aaaa", "lsr-base-amd64-aaaa"}, // the current key's, though older and unlabelled
		{"amd64", "gggg", "lsr-base-amd64-gggg"}, // the current key's, whatever its label says
		{"amd64", "cccc", "lsr-base-amd64-bbbb"}, // still being made: the newest ready of the same programs
		{"amd64", "dddd", "lsr-base-amd64-bbbb"}, // failed: the newest ready of the same programs
		{"amd64", "ffff", "lsr-base-amd64-bbbb"}, // not made yet
		{"amd64", "", "lsr-base-amd64-hhhh"},     // no key: the newest ready of any
		{"arm64", "ffff", "lsr-base-arm64-eeee"}, // never another architecture's
		{"arm64", "eeee", "lsr-base-arm64-eeee"},
	} {
		got, err := bases.Boot(ims, tc.arch, tc.key, progs)
		if got != tc.want || err != nil {
			t.Errorf("Boot(%s, %s) = %s, %v; want %s", tc.arch, tc.key, got, err, tc.want)
		}
	}
	// With only images of other programs, or none recorded, there's none.
	for _, tc := range []struct {
		ims  []compute.Image
		arch string
		why  string
	}{
		{ims[6:7], "amd64", "lsr-base-amd64-gggg, was made with programs ffffffffffffffffffff, not the service's, " + progs},
		{ims[:1], "amd64", "lsr-base-amd64-aaaa, was made with no record of its programs"},
		{ims[2:4], "amd64", "lsr-base-amd64-ffff isn't ready, and there's no other ready base image of amd64"},
	} {
		got, err := bases.Boot(tc.ims, tc.arch, "ffff", progs)
		if got != "" || !errors.Is(err, bases.ErrNoImage) || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("Boot(%s) of %d images = %s, %v; want none, as %q", tc.arch, len(tc.ims), got, err, tc.why)
		}
	}
	if got, err := bases.Boot(ims[:1], "arm64", "eeee", progs); err == nil || got != "" {
		t.Errorf("Boot with no arm64 image = %s, %v", got, err)
	}
	if got, err := bases.Boot(ims[:1], "arm64", "", ""); !errors.Is(err, bases.ErrNoImage) || got != "" {
		t.Errorf("Boot with no arm64 image and no key = %s, %v", got, err)
	}
	if bases.ProgramsLabel(machine.Digest()) != machine.Digest()[:20] {
		t.Error("the programs' label isn't the digest's first 20 hex digits")
	}
}

// TestABaseImageIsMadeWhenTheKeyChanges follows a build from its builder to
// its image, while machines keep booting the previous image
// (specs/0003-disks.md, Making a base image).
func TestABaseImageIsMadeWhenTheKeyChanges(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// The previous images were made with the same programs: the key
	// changed for another of its inputs, such as Debian's image.
	for _, arch := range config.Arches {
		name := "lsr-base-" + arch + "-0000000000000000000a"
		e.gce.AddLabelledImage(name, "", gceArch(arch), labelled(name, programs()))
	}

	e.pass()
	for _, arch := range config.Arches {
		b, ok := e.builder(arch)
		if !ok {
			t.Fatalf("no %s builder", arch)
		}
		// A standard machine with no account at all, from Debian's own
		// image, in the service's subnetwork, deleted at its maximum run
		// duration, running only the service's script.
		want := map[string]string{"amd64": "n4-standard-2", "arm64": "c4a-standard-2"}[arch]
		if b.MachineType != want || b.Zone != zoneA || b.Provisioning != "STANDARD" || b.ServiceAccount != "" || len(b.Scopes) != 0 ||
			b.Termination != "DELETE" || b.MaxRun != bases.MaxRunDuration || b.Subnetwork != "lanescape-runners" || !b.ExternalIP {
			t.Errorf("%s builder = %+v", arch, b)
		}
		if b.Labels[bases.LabelKind] != bases.KindBuilder || b.Labels[bases.LabelArch] != arch || b.Labels[bases.LabelKey] != e.keys[arch] {
			t.Errorf("%s builder's labels = %v", arch, b.Labels)
		}
		for _, d := range e.gce.Disks() {
			if d.Name == b.Name && (d.SizeGB != bases.BootDiskGB || !strings.HasSuffix(d.SourceImage, "/projects/debian-cloud/global/images/"+e.cfg.Base.Debian[arch])) {
				t.Errorf("%s builder's boot disk = %+v", arch, d)
			}
		}
		md := b.Metadata
		if md[bases.MetaStartupScript] != machine.BuildScript() || md[bases.MetaArch] != arch || md[bases.MetaKey] != e.keys[arch] ||
			md[bases.MetaRunnerVersion] != e.cfg.Runner.Version || md[bases.MetaRunnerSHA256] != e.cfg.Runner.SHA256[arch] ||
			md[bases.MetaGoVersion] != e.cfg.Base.Go.Version || md[bases.MetaGoSHA256] != e.cfg.Base.Go.SHA256[arch] {
			t.Errorf("%s builder's metadata isn't the script and its settings", arch)
		}
		for k := range md {
			if k == "jit-config" || k == "lsr-service" || strings.Contains(k, "token") {
				t.Errorf("%s builder has metadata %s", arch, k)
			}
		}
		// What the builder reads of its files, from its metadata server,
		// is the archive of what's embedded.
		req, _ := http.NewRequest(http.MethodGet, e.gce.MetadataURL(b.Zone, b.Name)+"/computeMetadata/v1/instance/attributes/"+bases.MetaFiles, nil)
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		raw, err := base64.StdEncoding.DecodeString(string(body))
		archive, _ := machine.Archive()
		if err != nil || !bytes.Equal(raw, archive) {
			t.Errorf("%s builder's lsr-files isn't the archive in base64", arch)
		}
		if names := tarNames(t, raw); !strings.Contains(names, "src/supervisor/main.go") || !strings.Contains(names, "programs") {
			t.Errorf("%s builder's files: %s", arch, names)
		}
		if r, ok := e.record(arch); !ok || r.Attempts != 1 {
			t.Errorf("%s record = %+v, %v", arch, r, ok)
		}
		// Until the new image is ready, machines boot the previous one.
		if got := e.boot(arch); got != "lsr-base-"+arch+"-0000000000000000000a" {
			t.Errorf("%s machines boot %s while the new image is made", arch, got)
		}
	}

	// A pass with no result yet changes nothing.
	e.pass()
	if len(e.builders()) != 2 {
		t.Fatalf("builders = %+v", e.builders())
	}

	// The builders say they built their images: each is recorded, stopped,
	// and its image made from its boot disk.
	for _, arch := range config.Arches {
		b, _ := e.builder(arch)
		e.say(b, e.built(arch))
	}
	e.pass()
	for _, arch := range config.Arches {
		b, ok := e.builder(arch)
		if !ok || b.Status != "TERMINATED" {
			t.Fatalf("%s builder after it built = %+v", arch, b)
		}
		r, _ := e.record(arch)
		if r.Built != b.ID {
			t.Errorf("%s record = %+v, builder %d", arch, r, b.ID)
		}
		im, ok := e.image(bases.ImageName(arch, e.keys[arch]))
		if !ok || im.Status != "READY" || im.SourceDisk != b.Name || im.Labels[bases.LabelKind] != bases.KindBase ||
			im.Labels[bases.LabelArch] != arch || im.Labels[bases.LabelKey] != e.keys[arch] || im.Labels[bases.LabelPrograms] != programs() {
			t.Fatalf("%s image = %+v, %v", arch, im, ok)
		}
		if got := e.boot(arch); got != bases.ImageName(arch, e.keys[arch]) {
			t.Errorf("%s machines boot %s once the new image is ready", arch, got)
		}
	}

	// Once the image is ready, the builder and the record go; the previous
	// image stays for a day.
	e.pass()
	if bs := e.builders(); len(bs) != 0 {
		t.Fatalf("builders left = %+v", bs)
	}
	for _, arch := range config.Arches {
		if _, ok := e.record(arch); ok {
			t.Errorf("%s record left", arch)
		}
		if _, ok := e.image("lsr-base-" + arch + "-0000000000000000000a"); !ok {
			t.Errorf("%s previous image deleted at once", arch)
		}
	}
	e.clock.Advance(bases.Kept)
	e.pass()
	for _, arch := range config.Arches {
		if _, ok := e.image("lsr-base-" + arch + "-0000000000000000000a"); ok {
			t.Errorf("%s previous image kept a day after the new one", arch)
		}
		if _, ok := e.image(bases.ImageName(arch, e.keys[arch])); !ok {
			t.Errorf("%s current image deleted", arch)
		}
	}
	// Nothing more is made.
	e.pass()
	if bs := e.builders(); len(bs) != 0 {
		t.Fatalf("builders after the images = %+v", bs)
	}
	_ = ctx
}

func tarNames(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	return strings.Join(names, " ")
}

// ready makes the images of every architecture but one ready, so a test
// follows that one's build alone.
func (e *env) only(arch string) {
	for _, a := range config.Arches {
		if a != arch {
			e.gce.AddImage(bases.ImageName(a, e.keys[a]), "", map[string]string{"amd64": "X86_64", "arm64": "ARM64"}[a])
		}
	}
}

// TestAFailedBuildIsTriedAgainUpToThreeTimes: a builder that says it failed
// is deleted, with its console saved and its failure recorded, and the next
// pass makes another, up to three in all.
func TestAFailedBuildIsTriedAgainUpToThreeTimes(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	ctx := context.Background()
	var ids []uint64
	for attempt := 1; attempt <= bases.Attempts; attempt++ {
		e.pass()
		b, ok := e.builder("amd64")
		if !ok {
			t.Fatalf("no builder for attempt %d", attempt)
		}
		ids = append(ids, b.ID)
		e.say(b, machine.Failed+"line 70 exited 22: curl -fsSL https://go.dev/dl/...")
		e.pass()
		if _, ok := e.builder("amd64"); ok {
			t.Fatalf("builder of failed attempt %d not deleted", attempt)
		}
		r, _ := e.record("amd64")
		if r.Attempts != attempt || len(r.Failures) != attempt || r.Failures[attempt-1].Builder != b.ID ||
			!strings.Contains(r.Failures[attempt-1].Reason, "the builder failed: line 70 exited 22") {
			t.Fatalf("record after attempt %d = %+v", attempt, r)
		}
		data, _, err := e.objects.Get(ctx, "logs/"+b.Name+"-"+itoa(b.Created.Unix())+".txt")
		if err != nil || !strings.Contains(string(data), "line 70 exited 22") {
			t.Fatalf("failed builder's console: %q, %v", data, err)
		}
		e.clock.Advance(time.Minute)
	}
	if ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatalf("builders weren't new: %v", ids)
	}
	// After the third, no more for that key, and an error at each pass.
	e.pass()
	e.pass()
	if bs := e.builders(); len(bs) != 0 {
		t.Fatalf("a fourth builder: %+v", bs)
	}
	if n := strings.Count(e.logs.String(), "making no more for this key"); n != 2 {
		t.Errorf("logged giving up %d times", n)
	}
	// The maintainer deleting the record starts again.
	_, gen, _ := e.objects.Get(ctx, "bases/amd64-"+e.keys["amd64"]+".json")
	if err := e.objects.Delete(ctx, "bases/amd64-"+e.keys["amd64"]+".json", gen); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if _, ok := e.builder("amd64"); !ok {
		t.Fatal("no builder once the record was deleted")
	}
}

// bootErr is why machines of an architecture boot nothing now.
func (e *env) bootErr(arch string) error {
	e.t.Helper()
	ims, err := e.gcec.Images(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	name, err := e.svc.Boot(ims, arch)
	if err == nil {
		e.t.Fatalf("%s machines boot %s", arch, name)
	}
	if !errors.Is(err, bases.ErrNoImage) {
		e.t.Fatalf("%s machines boot nothing for %v, not for want of an image", arch, err)
	}
	return err
}

// TestMachinesWaitForAnImageOfTheServicesPrograms: while a key's image is
// made, machines boot no earlier image made with other programs, or with
// none recorded, as images made before they had the label: nothing boots
// until the new image is ready, which the one builder goes on making
// (specs/0003-disks.md, Which image machines boot).
func TestMachinesWaitForAnImageOfTheServicesPrograms(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	unlabelled := "lsr-base-amd64-0000000000000000000a"
	e.gce.AddImage(unlabelled, "", "X86_64")
	e.clock.Advance(time.Second)
	stale := "lsr-base-amd64-0000000000000000000b"
	e.gce.AddLabelledImage(stale, "", "X86_64", labelled(stale, "ffffffffffffffffffff"))

	e.pass()
	b, ok := e.builder("amd64")
	if !ok {
		t.Fatal("no builder")
	}
	err := e.bootErr("amd64")
	if !strings.Contains(err.Error(), stale+", was made with programs ffffffffffffffffffff, not the service's, "+programs()) ||
		!strings.Contains(err.Error(), "its jobs wait for "+bases.ImageName("amd64", e.keys["amd64"])) {
		t.Errorf("why = %v", err)
	}
	// Without the label, the older image is no more use.
	e.gce.DeleteImageNow(stale)
	if err := e.bootErr("amd64"); !strings.Contains(err.Error(), unlabelled+", was made with no record of its programs") {
		t.Errorf("why = %v", err)
	}

	// Waiting makes no more builders.
	for range 3 {
		e.pass()
	}
	if bs := e.builders(); len(bs) != 1 || bs[0].ID != b.ID {
		t.Fatalf("builders while machines wait = %+v", bs)
	}
	if r, _ := e.record("amd64"); r.Attempts != 1 || len(r.Failures) != 0 {
		t.Fatalf("record = %+v", r)
	}

	// Once the image is ready, machines boot it.
	e.say(b, e.built("amd64"))
	e.pass()
	if got := e.boot("amd64"); got != bases.ImageName("amd64", e.keys["amd64"]) {
		t.Fatalf("machines boot %q once the image is ready", got)
	}
}

// TestTheCurrentImageIsLabelledWithItsPrograms: the current key's image,
// if made before images had the programs' label, is labelled with the
// service's programs, which its key covers, so that a later key's build
// with the same programs boots it meanwhile, and one with other programs
// doesn't.
func TestTheCurrentImageIsLabelledWithItsPrograms(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	cur := bases.ImageName("amd64", e.keys["amd64"])
	e.gce.AddLabelledImage(cur, "", "X86_64", map[string]string{bases.LabelKind: bases.KindBase, "other": "kept"})
	// A label that can't be set is set at a later pass.
	e.gce.ServerError(http.MethodPost, "/global/images/"+cur+"/setLabels", http.StatusForbidden, 1)
	e.pass()
	if im, _ := e.image(cur); im.Labels[bases.LabelPrograms] != "" {
		t.Fatalf("labels = %v; the label was to fail", im.Labels)
	}
	e.pass()
	im, _ := e.image(cur)
	if im.Labels[bases.LabelPrograms] != programs() || im.Labels["other"] != "kept" || im.Labels[bases.LabelKind] != bases.KindBase {
		t.Fatalf("labels = %v", im.Labels)
	}
	if !strings.Contains(e.logs.String(), "base image labelled with its programs") {
		t.Error("labelling not logged")
	}
	// A label of other programs on the current key's image, which its key
	// says are the service's, is set right.
	got, err := e.gcec.Image(context.Background(), cur)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.gcec.SetImageLabels(context.Background(), cur, labelled(cur, "ffffffffffffffffffff"), got.LabelFingerprint); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if im, _ := e.image(cur); im.Labels[bases.LabelPrograms] != programs() {
		t.Fatalf("labels = %v; want the service's programs", im.Labels)
	}
	ims, err := e.gcec.Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	later := map[string]string{"amd64": "eeeeeeeeeeeeeeeeeeee", "arm64": e.keys["arm64"]}
	same := &bases.Service{Config: e.cfg, Keys: later}
	if got, err := same.Boot(ims, "amd64"); got != cur || err != nil {
		t.Fatalf("a later key with the same programs boots %q, %v", got, err)
	}
	changed := &bases.Service{Config: e.cfg, Keys: later, Programs: strings.Repeat("f", 64)}
	if got, err := changed.Boot(ims, "amd64"); got != "" || !errors.Is(err, bases.ErrNoImage) {
		t.Fatalf("a later key with other programs boots %q, %v", got, err)
	}
}

// TestJobsWaitWhileAKeysBuildsHaveFailed: with every builder of a key
// failed and no earlier image of the service's programs, machines boot
// nothing: the error logged at every pass says so, and no more builders are
// made until the maintainer deletes the key's record (specs/0003-disks.md,
// Making a base image).
func TestJobsWaitWhileAKeysBuildsHaveFailed(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	ctx := context.Background()
	stale := "lsr-base-amd64-0000000000000000000b"
	e.gce.AddLabelledImage(stale, "", "X86_64", labelled(stale, "ffffffffffffffffffff"))
	for range bases.Attempts {
		e.pass()
		b, ok := e.builder("amd64")
		if !ok {
			t.Fatal("no builder")
		}
		e.say(b, machine.Failed+"no network")
		e.pass()
		e.clock.Advance(time.Minute)
	}
	e.pass()
	e.pass()
	if bs := e.builders(); len(bs) != 0 {
		t.Fatalf("a fourth builder: %+v", bs)
	}
	e.bootErr("amd64")
	logs := e.logs.String()
	if n := strings.Count(logs, "no machine of its architecture until a deploy changes the key or the maintainer deletes its record"); n != 2 {
		t.Errorf("logged giving up, with no machine to boot, %d times in two passes", n)
	}
	if !strings.Contains(logs, "machines_boot=none") {
		t.Error("the log doesn't say machines boot nothing")
	}
	_, gen, _ := e.objects.Get(ctx, "bases/amd64-"+e.keys["amd64"]+".json")
	if err := e.objects.Delete(ctx, "bases/amd64-"+e.keys["amd64"]+".json", gen); err != nil {
		t.Fatal(err)
	}
	e.pass()
	b, ok := e.builder("amd64")
	if !ok {
		t.Fatal("no builder once the record was deleted")
	}
	e.say(b, e.built("amd64"))
	e.pass()
	if got := e.boot("amd64"); got != bases.ImageName("amd64", e.keys["amd64"]) {
		t.Fatalf("machines boot %q", got)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestABuilderThatNeverSaysFails: a builder that hasn't said it built the
// image in 60 minutes fails.
func TestABuilderThatNeverSaysFails(t *testing.T) {
	e := setup(t)
	e.only("arm64")
	e.pass()
	b, ok := e.builder("arm64")
	if !ok {
		t.Fatal("no builder")
	}
	e.gce.WriteSerial(b.Zone, b.Name, "still going\n")
	e.clock.Advance(bases.ReportLimit - time.Minute)
	e.pass()
	if _, ok := e.builder("arm64"); !ok {
		t.Fatal("builder deleted before its limit")
	}
	e.clock.Advance(time.Minute)
	e.pass()
	if _, ok := e.builder("arm64"); ok {
		t.Fatal("builder kept past its limit")
	}
	r, _ := e.record("arm64")
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0].Reason, "within 60 minutes") {
		t.Fatalf("record = %+v", r)
	}
}

// TestABuilderThatStopsBeforeItSaysFails: a builder that stops or goes
// before its success is recorded fails, and its stopped disk isn't imaged.
func TestABuilderThatStopsBeforeItSaysFails(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.pass()
	b, _ := e.builder("amd64")
	e.gce.Shutdown(b.Zone, b.Name)
	e.pass()
	if _, ok := e.builder("amd64"); ok {
		t.Fatal("stopped builder kept")
	}
	if _, ok := e.image(bases.ImageName("amd64", e.keys["amd64"])); ok {
		t.Fatal("an image made from a builder that never said it built it")
	}
	r, _ := e.record("amd64")
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0].Reason, "stopped before") {
		t.Fatalf("record = %+v", r)
	}
	// One whose success is recorded but that goes before its image is
	// made is tried again too.
	e.pass()
	b, _ = e.builder("amd64")
	e.gce.FailNextOperation("images.insert", "INTERNAL_ERROR")
	e.say(b, e.built("amd64"))
	e.pass()
	if got, _ := e.builder("amd64"); got.Status != "TERMINATED" {
		t.Fatalf("builder = %+v", got)
	}
	if err := e.gcec.DeleteInstance(context.Background(), b.Zone, b.Name, compute.RequestID("someone")); err != nil {
		t.Fatal(err)
	}
	e.pass()
	e.pass()
	b2, ok := e.builder("amd64")
	if !ok || b2.ID == b.ID {
		t.Fatalf("no new builder: %+v", b2)
	}
	r, _ = e.record("amd64")
	if r.Attempts != 3 || len(r.Failures) != 2 || r.Failures[1].Builder != b.ID || !strings.Contains(r.Failures[1].Reason, "went before") {
		t.Fatalf("record = %+v; the builder that went is to be recorded as failed", r)
	}
}

// TestAnImageNotReadyInTimeFails: an image Compute Engine is still making
// two hours after its builder was made fails the build, and the next
// builder starts again.
func TestAnImageNotReadyInTimeFails(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.gce.HoldImages(true)
	e.pass()
	b, _ := e.builder("amd64")
	e.say(b, e.built("amd64"))
	e.pass()
	name := bases.ImageName("amd64", e.keys["amd64"])
	if im, ok := e.image(name); !ok || im.Status != "PENDING" {
		t.Fatalf("image = %+v, %v", im, ok)
	}
	e.clock.Advance(bases.BuildLimit - time.Minute)
	e.pass()
	if _, ok := e.builder("amd64"); !ok {
		t.Fatal("builder deleted within the build's limit")
	}
	e.clock.Advance(time.Minute)
	e.pass()
	if _, ok := e.builder("amd64"); ok {
		t.Fatal("stopped builder kept past the build's limit")
	}
	if _, ok := e.image(name); ok {
		t.Fatal("late image kept")
	}
	r, _ := e.record("amd64")
	if len(r.Failures) != 1 || r.Failures[0].Builder != b.ID || !strings.Contains(r.Failures[0].Reason, "within 2 hours") {
		t.Fatalf("record = %+v", r)
	}
	e.gce.HoldImages(false)
	e.pass()
	if b2, ok := e.builder("amd64"); !ok || b2.ID == b.ID {
		t.Fatalf("no new builder: %+v", b2)
	}
	if r, _ := e.record("amd64"); r.Attempts != 2 || len(r.Failures) != 1 {
		t.Fatalf("record = %+v", r)
	}
}

// TestAFailedImageIsMadeAgain: Compute Engine can fail an image and leave it
// behind; it's deleted and made again from the stopped builder.
func TestAFailedImageIsMadeAgain(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.pass()
	b, _ := e.builder("amd64")
	e.say(b, e.built("amd64"))
	e.gce.FailNextOperation("images.insert.leave", "INTERNAL_ERROR")
	e.pass()
	name := bases.ImageName("amd64", e.keys["amd64"])
	if im, ok := e.image(name); !ok || im.Status != "FAILED" {
		t.Fatalf("image = %+v, %v", im, ok)
	}
	e.pass()
	if _, ok := e.image(name); ok {
		t.Fatal("failed image kept")
	}
	e.pass()
	if im, ok := e.image(name); !ok || im.Status != "READY" {
		t.Fatalf("image made again = %+v, %v", im, ok)
	}
	e.pass()
	if _, ok := e.builder("amd64"); ok {
		t.Fatal("builder kept once its image was ready")
	}
	if r, ok := e.record("amd64"); ok {
		t.Fatalf("record kept: %+v", r)
	}
}

// TestNoCapacityTakesTheAttemptBack: a builder goes to the next machine
// type or zone with capacity; with none, no attempt is counted.
func TestNoCapacityTakesTheAttemptBack(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	for _, z := range []string{zoneA, zoneB} {
		for _, mt := range e.cfg.Base.Builders["amd64"] {
			e.gce.Stockout(z, mt, false)
		}
	}
	e.pass()
	e.pass()
	if bs := e.builders(); len(bs) != 0 {
		t.Fatalf("builders with no capacity = %+v", bs)
	}
	if r, _ := e.record("amd64"); r.Attempts != 0 {
		t.Fatalf("record with no capacity = %+v", r)
	}
	// A stockout holds its machine families back from jobs for the pass.
	if !slices.Equal(e.short, []string{"n4", "c4"}) {
		t.Fatalf("held from jobs: %v", e.short)
	}
	// Short of quota, which every family may share, it holds every job.
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 0)
	e.gce.SetQuota("northamerica-northeast1", "C4_CPUS", 0)
	e.pass()
	if !slices.Equal(e.short, []string{bases.AllPools}) {
		t.Fatalf("held from jobs short of quota: %v", e.short)
	}
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 100)
	e.gce.SetQuota("northamerica-northeast1", "C4_CPUS", 100)
	e.gce.ClearStockout(zoneB, "n4-standard-2", false)
	e.pass()
	if e.short != nil {
		t.Fatalf("held from jobs once the builder was made: %v", e.short)
	}
	b, ok := e.builder("amd64")
	if !ok || b.Zone != zoneB || b.MachineType != "n4-standard-2" {
		t.Fatalf("builder = %+v, %v", b, ok)
	}
	if r, _ := e.record("amd64"); r.Attempts != 1 {
		t.Fatalf("record = %+v", r)
	}
}

// TestALostAnswerStillCountsTheBuilder: a builder whose making's answer
// was lost counted before it was made, and the next pass finds it.
func TestALostAnswerStillCountsTheBuilder(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.gce.LoseResponse(http.MethodPost, "/zones/"+zoneA+"/instances", 10)
	e.pass()
	if r, _ := e.record("amd64"); r.Attempts != 1 {
		t.Fatalf("record = %+v", r)
	}
	if _, ok := e.builder("amd64"); !ok {
		t.Fatal("no builder")
	}
	e.gce.Faults().Reset()
	e.pass()
	if bs := e.builders(); len(bs) != 1 {
		t.Fatalf("builders = %+v", bs)
	}
	if r, _ := e.record("amd64"); r.Attempts != 1 {
		t.Fatalf("record = %+v", r)
	}
}

// TestAnotherKeysBuildIsLeftWhileItMightGoOn: a builder of another key, as
// a revision a deploy replaced makes, goes only once it's past any build's
// limit, and its record a day after it was last written.
func TestAnotherKeysBuildIsLeftWhileItMightGoOn(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.gce.AddImage(bases.ImageName("amd64", e.keys["amd64"]), "", "X86_64")
	ctx := context.Background()
	other := bases.BuilderName("amd64", "ffffffffffffffffffff")
	if err := e.gcec.CreateInstance(ctx, zoneA, compute.InstanceSpec{Name: other, MachineType: "n4-standard-2", Image: e.cfg.Base.Debian["amd64"],
		ImageProject: config.DebianProject, BootDiskGB: 10, DiskType: "hyperdisk-balanced", Network: "lanescape-runners",
		Labels: map[string]string{bases.LabelKind: bases.KindBuilder}}, compute.RequestID("other")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.objects.Create(ctx, "bases/amd64-ffffffffffffffffffff.json", []byte(`{"arch":"amd64","key":"ffffffffffffffffffff","attempts":1}`)); err != nil {
		t.Fatal(err)
	}
	gone := func(name string) bool {
		for _, in := range e.gce.Instances() {
			if in.Name == name {
				return false
			}
		}
		return true
	}
	e.pass()
	e.clock.Advance(bases.BuildLimit - time.Minute)
	e.pass()
	if gone(other) {
		t.Fatal("another key's builder deleted while its build might go on")
	}
	e.clock.Advance(time.Minute)
	e.pass()
	if !gone(other) {
		t.Fatal("another key's builder kept past any build's limit")
	}
	if _, _, err := e.objects.Get(ctx, "bases/amd64-ffffffffffffffffffff.json"); err != nil {
		t.Fatal("another key's record deleted within a day")
	}
	e.clock.Advance(bases.Kept)
	e.pass()
	if _, _, err := e.objects.Get(ctx, "bases/amd64-ffffffffffffffffffff.json"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another key's record kept a day on: %v", err)
	}
}

// TestARevertedDeployBootsItsEarlierImage: other base images go only once
// both they and the current one are a day old, so a deploy reverted within
// a day finds its image still there.
func TestARevertedDeployBootsItsEarlierImage(t *testing.T) {
	e := setup(t)
	e.only("arm64")
	cur := bases.ImageName("arm64", e.keys["arm64"])
	e.gce.AddImage(cur, "", "ARM64")
	e.clock.Advance(2 * bases.Kept)
	// A later key's image, as a deploy since reverted made.
	later := "lsr-base-arm64-eeeeeeeeeeeeeeeeeeee"
	e.gce.AddImage(later, "", "ARM64")
	e.pass()
	if _, ok := e.image(later); !ok {
		t.Fatal("a reverted deploy's image deleted within a day")
	}
	if got := e.boot("arm64"); got != cur {
		t.Fatalf("machines boot %s, not the current key's %s", got, cur)
	}
	e.clock.Advance(bases.Kept)
	e.pass()
	if _, ok := e.image(later); ok {
		t.Fatal("a reverted deploy's image kept a day on")
	}
	if _, ok := e.image(cur); !ok {
		t.Fatal("the current image deleted")
	}
}

// TestAFailureIsCountedOncePerBuilder: a builder whose deletion didn't
// finish isn't failed again.
func TestAFailureIsCountedOncePerBuilder(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.pass()
	b, _ := e.builder("amd64")
	e.say(b, machine.Failed+"no network")
	e.gce.FailNextOperation("instances.delete", "INTERNAL_ERROR")
	e.pass()
	if _, ok := e.builder("amd64"); !ok {
		t.Fatal("the builder's deletion was to fail")
	}
	e.pass()
	if _, ok := e.builder("amd64"); ok {
		t.Fatal("builder not deleted the next time")
	}
	if r, _ := e.record("amd64"); len(r.Failures) != 1 || r.Attempts != 1 {
		t.Fatalf("record = %+v", r)
	}
}

// TestAFailedImageThatWontGoStillEndsTheBuild: a failed image whose deletion
// keeps failing doesn't hold the build past its limit.
func TestAFailedImageThatWontGoStillEndsTheBuild(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.pass()
	b, _ := e.builder("amd64")
	e.say(b, e.built("amd64"))
	e.gce.FailNextOperation("images.insert.leave", "INTERNAL_ERROR")
	e.pass()
	name := bases.ImageName("amd64", e.keys["amd64"])
	e.gce.ServerError(http.MethodDelete, "/global/images/"+name, http.StatusForbidden, 1000)
	e.pass()
	if im, ok := e.image(name); !ok || im.Status != "FAILED" {
		t.Fatalf("image = %+v, %v", im, ok)
	}
	e.clock.Advance(bases.BuildLimit)
	e.pass()
	if _, ok := e.builder("amd64"); ok {
		t.Fatal("builder kept past the build's limit")
	}
	r, _ := e.record("amd64")
	if len(r.Failures) != 1 || r.Failures[0].Builder != b.ID || !strings.Contains(r.Failures[0].Reason, "within 2 hours") {
		t.Fatalf("record = %+v", r)
	}
}

// TestABuilderNoPassSawStillFails: a builder that went before any pass saw
// it, such as one deleted at its maximum run duration while the service
// couldn't act, ends its attempt as a failure.
func TestABuilderNoPassSawStillFails(t *testing.T) {
	e := setup(t)
	e.only("arm64")
	e.pass()
	b, _ := e.builder("arm64")
	if err := e.gcec.DeleteInstance(context.Background(), b.Zone, b.Name, compute.RequestID("max run")); err != nil {
		t.Fatal(err)
	}
	e.pass()
	r, _ := e.record("arm64")
	if r.Attempts != 2 || len(r.Failures) != 1 || r.Failures[0].Builder != 0 || !strings.Contains(r.Failures[0].Reason, "before a pass saw it") {
		t.Fatalf("record = %+v", r)
	}
	if b2, ok := e.builder("arm64"); !ok || b2.ID == b.ID {
		t.Fatalf("no new builder: %+v", b2)
	}
}

// TestABuildsLimitRunsFromItsAttempt: a builder that goes while its image is
// still being made doesn't move the build's limit, which runs from when its
// attempt was counted.
func TestABuildsLimitRunsFromItsAttempt(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.gce.HoldImages(true)
	e.pass()
	b, _ := e.builder("amd64")
	e.clock.Advance(50 * time.Minute)
	e.say(b, e.built("amd64"))
	e.pass()
	name := bases.ImageName("amd64", e.keys["amd64"])
	if im, ok := e.image(name); !ok || im.Status != "PENDING" {
		t.Fatalf("image = %+v, %v", im, ok)
	}
	if err := e.gcec.DeleteInstance(context.Background(), b.Zone, b.Name, compute.RequestID("someone")); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(70 * time.Minute)
	e.pass()
	if _, ok := e.image(name); ok {
		t.Fatal("the image kept past its build's limit")
	}
	r, _ := e.record("amd64")
	if len(r.Failures) != 1 || r.Failures[0].Builder != b.ID || !strings.Contains(r.Failures[0].Reason, "within 2 hours") {
		t.Fatalf("record = %+v", r)
	}
}

// TestGivingUpIsLoggedWhateverTheImage: once a key's builders have all
// failed, the error is logged at every pass, even while a failed image of
// it won't go.
func TestGivingUpIsLoggedWhateverTheImage(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	for i := 0; i < bases.Attempts-1; i++ {
		e.pass()
		b, _ := e.builder("amd64")
		e.say(b, machine.Failed+"no network")
		e.pass()
		e.clock.Advance(time.Minute)
	}
	e.pass()
	b, _ := e.builder("amd64")
	e.say(b, e.built("amd64"))
	e.gce.FailNextOperation("images.insert.leave", "INTERNAL_ERROR")
	e.pass()
	name := bases.ImageName("amd64", e.keys["amd64"])
	// Neither the failed image nor the builder will go.
	e.gce.ServerError(http.MethodDelete, "/global/images/"+name, http.StatusForbidden, 1000)
	e.gce.ServerError(http.MethodDelete, "/instances/"+b.Name, http.StatusForbidden, 1000)
	e.clock.Advance(bases.BuildLimit)
	e.pass()
	e.pass()
	if _, ok := e.builder("amd64"); !ok {
		t.Fatal("the builder was to linger")
	}
	if r, _ := e.record("amd64"); r.Attempts != bases.Attempts || len(r.Failures) != bases.Attempts {
		t.Fatalf("record = %+v", r)
	}
	if n := strings.Count(e.logs.String(), "making no more for this key"); n != 2 {
		t.Fatalf("logged giving up %d times in two passes", n)
	}
	e.pass()
	if n := strings.Count(e.logs.String(), "making no more for this key"); n != 3 {
		t.Fatalf("logged giving up %d times in three passes", n)
	}
}

// TestAConsoleThatCantBeReadIsntSilence: a builder whose console a pass
// can't read isn't judged silent; the next pass that reads it finds its
// success.
func TestAConsoleThatCantBeReadIsntSilence(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	e.pass()
	b, _ := e.builder("amd64")
	e.say(b, e.built("amd64"))
	e.clock.Advance(bases.ReportLimit + 5*time.Minute)
	e.gce.ServerError(http.MethodGet, "/serialPort", http.StatusServiceUnavailable, 1)
	e.pass()
	if got, ok := e.builder("amd64"); !ok || got.ID != b.ID {
		t.Fatal("a builder whose console couldn't be read was failed")
	}
	e.pass()
	if im, ok := e.image(bases.ImageName("amd64", e.keys["amd64"])); !ok || im.Status != "READY" {
		t.Fatalf("image = %+v, %v", im, ok)
	}
	if r, _ := e.record("amd64"); len(r.Failures) != 0 {
		t.Fatalf("record = %+v", r)
	}
}

// TestABuildsLimitRunsFromItsAttemptWithNoImage: with no image yet, the
// build's limit still runs from its attempt's start, not its builder's.
func TestABuildsLimitRunsFromItsAttemptWithNoImage(t *testing.T) {
	e := setup(t)
	e.only("amd64")
	ctx := context.Background()
	e.pass()
	b, _ := e.builder("amd64")
	// The attempt was counted 10 minutes before its builder was made.
	key := "bases/amd64-" + e.keys["amd64"] + ".json"
	data, gen, err := e.objects.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var r bases.Record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	r.Started = r.Started.Add(-10 * time.Minute)
	data, _ = json.Marshal(r)
	if _, err := e.objects.Replace(ctx, key, data, gen); err != nil {
		t.Fatal(err)
	}
	e.say(b, e.built("amd64"))
	e.gce.FailNextOperation("images.insert", "INTERNAL_ERROR")
	e.pass()
	if _, ok := e.image(bases.ImageName("amd64", e.keys["amd64"])); ok {
		t.Fatal("an image was to fail to be made")
	}
	e.clock.Advance(bases.BuildLimit - 10*time.Minute)
	e.pass()
	if _, ok := e.builder("amd64"); ok {
		t.Fatal("builder kept past its attempt's limit")
	}
	if r, _ := e.record("amd64"); len(r.Failures) != 1 || !strings.Contains(r.Failures[0].Reason, "within 2 hours") {
		t.Fatalf("record = %+v", r)
	}
}
