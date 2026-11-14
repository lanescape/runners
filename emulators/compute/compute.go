// Package compute emulates the parts of Compute Engine the service uses, as
// Google's REST client (google.golang.org/api/compute/v1) calls them over
// HTTP: instances, with their labels, metadata, guest attributes and serial
// output; disks, attached and detached; images; and zone and global
// operations. It provisions standard and Spot machines, preempts Spot ones as
// Compute Engine does, with a compute.instances.preempted zone operation
// whose targetId is the instance's numeric ID, and fails as Compute Engine
// does: stockouts and quota errors per zone and machine type, duplicate
// requests answered by their requestId, and responses lost after the work was
// done (through Faults).
//
// Each instance has a metadata server of its own (MetadataURL), which serves
// its attributes, takes its guest attributes, and issues its identity tokens
// in the full format, signed by the emulator's own keys (Identity). Tests
// play a machine's boot and shutdown through OnBoot and OnStop.
package compute

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"google.golang.org/api/option"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/oidc"
)

// Emulator is a running Compute Engine emulator for one project.
type Emulator struct {
	// URL is the API's base address, with no trailing slash.
	URL string
	// Identity issues instances' identity tokens, as Google does.
	Identity *oidc.Issuer
	// Project and ProjectNumber name the one project the emulator serves.
	Project       string
	ProjectNumber int64

	srv    *httptest.Server
	md     *httptest.Server
	faults fault.Injector
	clock  clockwork.Clock
	zones  []string

	mu        sync.Mutex
	instances map[string]*instance // by zone/name
	disks     map[string]*disk     // by zone/name
	images    map[string]*image    // by name
	public    map[string]*image    // other projects' public images, by project/name
	ops       map[string]*operation
	requests  map[string]*operation // by requestId
	stockouts map[capacityKey]bool
	refusing  map[capacityKey]bool
	diskShort map[string]bool
	failNext  map[string]string
	quotas    map[string]int64 // by region/metric
	opDelay   time.Duration
	// pendingImages makes new images stay PENDING.
	pendingImages bool
	onBoot        func(Machine)
	onStop        func(Machine)
	booted        map[uint64]bool
	// logged is each instance's serial console as Cloud Logging keeps it,
	// by instance ID, line by line, after the instance is gone too.
	logged map[uint64][]logLine
	// LogDelay holds back each line from Cloud Logging for that long after
	// it was written, as Cloud Logging's delivery does.
	logDelay time.Duration
}

type logLine struct {
	text string
	at   time.Time
}

type capacityKey struct {
	zone, machineType string
	spot              bool
}

// Machine is an instance as its own guest sees it.
type Machine struct {
	Zone string
	Name string
	ID   uint64
	// MetadataURL is the base address of its metadata server, in place of
	// http://metadata.google.internal.
	MetadataURL string
}

type attached struct {
	deviceName string
	disk       *disk
	boot       bool
	autoDelete bool
	index      int64
}

type instance struct {
	id             uint64
	name, zone     string
	machineType    string
	status         string
	created        time.Time
	readyAt        time.Time
	labels         map[string]string
	labelFP        int64
	metadata       []metadataItem
	metaFP         int64
	disks          []*attached
	network        string
	subnetwork     string
	externalIP     bool
	serviceAccount string
	scopes         []string
	provisioning   string
	termination    string
	maxRun         time.Duration
	hostMaint      string
	started        time.Time
	stopped        time.Time
	guest          map[string]string // namespace/key
	serial         strings.Builder
}

type metadataItem struct{ key, value string }

type disk struct {
	id          uint64
	name, zone  string
	typ         string
	sizeGB      int64
	iops        int64
	throughput  int64
	labels      map[string]string
	sourceImage string
	arch        string
	created     time.Time
	users       []*instance
}

type image struct {
	id         uint64
	project    string // another project's, for a public image
	name       string
	family     string
	status     string
	arch       string
	labels     map[string]string
	created    time.Time
	sourceDisk string
	sizeGB     int64
	labelFP    int64
}

// Option configures an Emulator.
type Option func(*Emulator)

// WithClock sets the clock that stamps resources and operations, and that
// maximum run durations count by.
func WithClock(c clockwork.Clock) Option { return func(e *Emulator) { e.clock = c } }

// WithProject sets the project's ID and number.
func WithProject(id string, number int64) Option {
	return func(e *Emulator) { e.Project, e.ProjectNumber = id, number }
}

// WithZones sets the zones the project has.
func WithZones(zones ...string) Option { return func(e *Emulator) { e.zones = zones } }

// New starts an emulator. Close it when done.
func New(opts ...Option) *Emulator {
	e := &Emulator{
		Project: "lanescape-dev", ProjectNumber: 123456789012,
		clock:     clockwork.NewRealClock(),
		zones:     []string{"northamerica-northeast1-a", "northamerica-northeast1-b"},
		instances: map[string]*instance{}, disks: map[string]*disk{}, images: map[string]*image{}, public: map[string]*image{},
		ops: map[string]*operation{}, requests: map[string]*operation{},
		stockouts: map[capacityKey]bool{}, refusing: map[capacityKey]bool{}, diskShort: map[string]bool{}, failNext: map[string]string{}, quotas: map[string]int64{}, booted: map[uint64]bool{},
	}
	for _, o := range opts {
		o(e)
	}
	e.Identity = oidc.New(oidc.WithJWKSPath("/oauth2/v3/certs"), oidc.WithLifetime(time.Hour), oidc.WithClock(e.clock))
	e.srv = httptest.NewServer(e.faults.Wrap(e.routes()))
	e.URL = e.srv.URL
	e.md = httptest.NewServer(e.metadataRoutes())
	return e
}

// Close stops the emulator.
func (e *Emulator) Close() {
	e.srv.Close()
	e.md.Close()
	e.Identity.Close()
}

// Faults is where tests make API requests fail in other ways, such as
// losing a response after the work was done.
func (e *Emulator) Faults() *fault.Injector { return &e.faults }

// ClientOptions are what google.golang.org/api/compute/v1 needs to talk to
// the emulator instead of Google.
func (e *Emulator) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(e.URL + "/compute/v1/"), option.WithoutAuthentication()}
}

// LoggingOptions point a Cloud Logging client at the emulator, which serves
// its instances' serial consoles as Cloud Logging does.
func (e *Emulator) LoggingOptions() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(e.URL + "/"), option.WithoutAuthentication()}
}

// Zones are the project's zones.
func (e *Emulator) Zones() []string { return slices.Clone(e.zones) }

func newID() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])>>1 | 1<<40
}

func region(zone string) string { return zone[:strings.LastIndex(zone, "-")] }

var machineTypeName = regexp.MustCompile(`^([a-z][a-z0-9]*)-(standard|highcpu|highmem)-([1-9][0-9]*)$`)

// armFamilies are the machine families whose machines are Arm.
var armFamilies = map[string]bool{"c4a": true, "n4a": true, "t2a": true}

// hyperdiskOnly are the families that take only Hyperdisk.
var hyperdiskOnly = map[string]bool{"n4": true, "c4": true, "c4a": true, "n4a": true}

// machineShape is a predefined machine type's family and vCPUs.
func machineShape(mt string) (family string, vcpus int64, ok bool) {
	m := machineTypeName.FindStringSubmatch(mt)
	if m == nil {
		return "", 0, false
	}
	n, _ := strconv.ParseInt(m[3], 10, 64)
	return m[1], n, true
}

func archOf(family string) string {
	if armFamilies[family] {
		return "ARM64"
	}
	return "X86_64"
}

// AddImage adds a ready image to the project, as one built elsewhere would
// be. arch is X86_64 or ARM64.
func (e *Emulator) AddImage(name, family, arch string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.images[name] = &image{id: newID(), name: name, family: family, status: "READY", arch: arch, created: e.clock.Now(), sizeGB: 10}
}

// HoldImages makes images made from now on stay PENDING, as one Compute
// Engine is slow to finish does, or lets them be READY again, those held
// included.
func (e *Emulator) HoldImages(hold bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pendingImages = hold
	if !hold {
		for _, im := range e.images {
			if im.status == "PENDING" {
				im.status = "READY"
			}
		}
	}
}

// AddPublicImage adds a ready image to another project, public, as
// Debian's own images in debian-cloud are, which any machine may boot.
func (e *Emulator) AddPublicImage(project, name, arch string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.public[project+"/"+name] = &image{id: newID(), project: project, name: name, status: "READY", arch: arch, created: e.clock.Now(), sizeGB: 10}
}

// Stockout makes a zone have no capacity for a machine type, standard or
// Spot, until ClearStockout.
func (e *Emulator) Stockout(zone, machineType string, spot bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stockouts[capacityKey{zone, machineType, spot}] = true
}

// RefuseForCapacity makes a zone refuse requests for a machine type at
// once, with 503 and the reason insufficientCapacity, rather than in the
// operation, as Compute Engine sometimes does, until ClearStockout.
func (e *Emulator) RefuseForCapacity(zone, machineType string, spot bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refusing[capacityKey{zone, machineType, spot}] = true
}

// FailNextOperation makes the next operation of a kind, "instances.delete",
// "disks.insert", "images.insert" or "images.delete", or
// "images.insert.leave", which also leaves a FAILED image behind, finish with an error of code, such as INTERNAL_ERROR or
// RESOURCE_NOT_READY, having done nothing, as Compute Engine sometimes does
// after accepting a request.
func (e *Emulator) FailNextOperation(kind, code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext[kind] = code
}

// failing reports, once, whether the next operation of a kind fails.
func (e *Emulator) failing(kind string) (string, bool) {
	code, ok := e.failNext[kind]
	delete(e.failNext, kind)
	return code, ok
}

// StockoutDisks makes a zone have no capacity for new disks, or gives it
// back.
func (e *Emulator) StockoutDisks(zone string, short bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.diskShort[zone] = short
}

// ClearStockout gives capacity back.
func (e *Emulator) ClearStockout(zone, machineType string, spot bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.stockouts, capacityKey{zone, machineType, spot})
	delete(e.refusing, capacityKey{zone, machineType, spot})
}

// SetQuota limits a region's metric, such as N4_CPUS, C4A_CPUS or
// PREEMPTIBLE_CPUS, which Spot machines count against when it's set.
func (e *Emulator) SetQuota(region, metric string, limit int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotas[region+"/"+metric] = limit
}

// SetOperationDelay makes operations take d, in real time, before they're
// done, and machines as long to boot.
func (e *Emulator) SetOperationDelay(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opDelay = d
}

// OnBoot sets what runs, in its own goroutine, when an instance starts
// running: in tests, the machine's own programs.
func (e *Emulator) OnBoot(f func(Machine)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onBoot = f
}

// OnStop sets what runs when a running instance stops for any reason, or
// is deleted: in tests, the end of the machine's programs.
func (e *Emulator) OnStop(f func(Machine)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onStop = f
}

func (e *Emulator) machine(in *instance) Machine {
	return Machine{Zone: in.zone, Name: in.name, ID: in.id, MetadataURL: fmt.Sprintf("%s/%d", e.md.URL, in.id)}
}

// running reports whether an instance is up, and makes it so once its boot
// time has come. The caller holds e.mu.
func (e *Emulator) running(in *instance) bool {
	if in.status == "PROVISIONING" && !time.Now().Before(in.readyAt) {
		in.status = "RUNNING"
		in.started = e.clock.Now()
		e.boot(in)
	}
	return in.status == "RUNNING"
}

// boot runs the OnBoot hook once for an instance's run. The caller holds
// e.mu.
func (e *Emulator) boot(in *instance) {
	if e.onBoot != nil && !e.booted[in.id] {
		e.booted[in.id] = true
		f, m := e.onBoot, e.machine(in)
		go f(m)
	}
}

// halt stops a running instance's guest, and runs OnStop. The caller holds
// e.mu.
func (e *Emulator) halt(in *instance, status string) {
	was := in.status == "RUNNING" || in.status == "STOPPING"
	if was {
		in.stopped = e.clock.Now()
	}
	in.status = status
	delete(e.booted, in.id)
	if was && e.onStop != nil {
		f, m := e.onStop, e.machine(in)
		go f(m)
	}
}

// tick applies what time does: machines boot, and those past their maximum
// run duration stop or are deleted, as their termination action says. The
// caller holds e.mu.
func (e *Emulator) tick() {
	for _, in := range e.instances {
		e.running(in)
		if in.status == "RUNNING" && in.maxRun > 0 && e.clock.Now().Sub(in.started) >= in.maxRun {
			e.terminate(in, "compute.instances.terminateOnMaxRunDuration")
		}
	}
}

// terminate ends an instance as its termination action says, recording a
// system operation of type opType. The caller holds e.mu.
func (e *Emulator) terminate(in *instance, opType string) {
	e.systemOp(in, opType)
	if in.termination == "DELETE" {
		e.remove(in)
		return
	}
	e.halt(in, "TERMINATED")
}

// remove deletes an instance, its auto-delete disks with it. The caller
// holds e.mu.
func (e *Emulator) remove(in *instance) {
	e.halt(in, "TERMINATED")
	for _, a := range in.disks {
		a.disk.users = slices.DeleteFunc(a.disk.users, func(u *instance) bool { return u == in })
		if a.autoDelete && len(a.disk.users) == 0 {
			delete(e.disks, a.disk.zone+"/"+a.disk.name)
		}
	}
	delete(e.instances, in.zone+"/"+in.name)
}

// Preempt preempts a running Spot instance, as Compute Engine may at any
// time: it stops or is deleted, as its termination action says, and a
// compute.instances.preempted operation records it.
func (e *Emulator) Preempt(zone, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.instances[zone+"/"+name]
	if in == nil || !e.running(in) {
		return fmt.Errorf("compute emulator: %s/%s isn't running", zone, name)
	}
	if in.provisioning != "SPOT" {
		return fmt.Errorf("compute emulator: %s/%s is standard; only Spot machines are preempted", zone, name)
	}
	e.terminate(in, "compute.instances.preempted")
	return nil
}

// Shutdown stops an instance from inside, as its guest's poweroff does.
func (e *Emulator) Shutdown(zone, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if in := e.instances[zone+"/"+name]; in != nil && (in.status == "STOPPING" || e.running(in)) {
		e.systemOp(in, "compute.instances.guestTerminate")
		e.halt(in, "TERMINATED")
	}
}

// LogEntry is the Cloud Logging entry Compute Engine writes to the
// project's system event audit log for an instance, as a log sink publishes
// it: method is compute.instances.preempted or
// compute.instances.guestTerminate. It reports false if there's no such
// instance.
func (e *Emulator) LogEntry(zone, name, method string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.instances[zone+"/"+name]
	if in == nil {
		return nil, false
	}
	b, _ := json.Marshal(map[string]any{
		"protoPayload": map[string]any{
			"@type": "type.googleapis.com/google.cloud.audit.AuditLog", "serviceName": "compute.googleapis.com",
			"methodName":         method,
			"resourceName":       "projects/" + e.Project + "/zones/" + zone + "/instances/" + name,
			"authenticationInfo": map[string]any{"principalEmail": "system@google.com"},
		},
		"insertId": fmt.Sprintf("%x", newID()),
		"resource": map[string]any{"type": "gce_instance", "labels": map[string]string{
			"instance_id": strconv.FormatUint(in.id, 10), "project_id": e.Project, "zone": zone}},
		"timestamp":        e.clock.Now().UTC().Format(time.RFC3339Nano),
		"severity":         "INFO",
		"logName":          "projects/" + e.Project + "/logs/cloudaudit.googleapis.com%2Fsystem_event",
		"operation":        map[string]any{"id": fmt.Sprintf("systemevent-%d", in.id), "producer": "compute.googleapis.com", "first": true, "last": true},
		"receiveTimestamp": e.clock.Now().UTC().Format(time.RFC3339Nano),
	})
	return b, true
}

// WriteSerial adds to an instance's serial console output.
func (e *Emulator) WriteSerial(zone, name, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if in := e.instances[zone+"/"+name]; in != nil {
		e.writeSerial(in, text)
	}
}

// writeSerial adds to an instance's console, and to Cloud Logging's copy
// of it, line by line. The caller holds e.mu.
func (e *Emulator) writeSerial(in *instance, text string) {
	in.serial.WriteString(text)
	if e.logged == nil {
		e.logged = map[uint64][]logLine{}
	}
	for _, l := range strings.SplitAfter(text, "\n") {
		if l != "" {
			e.logged[in.id] = append(e.logged[in.id], logLine{text: strings.TrimSuffix(l, "\n"), at: e.clock.Now()})
		}
	}
}

// Serial is an instance's serial console output so far.
func (e *Emulator) Serial(zone, name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if in := e.instances[zone+"/"+name]; in != nil {
		return in.serial.String()
	}
	return ""
}

// SetLogDelay holds back serial console lines from Cloud Logging for d
// after they're written.
func (e *Emulator) SetLogDelay(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logDelay = d
}

// InstanceState is what tests see of an instance.
type InstanceState struct {
	Name, Zone, MachineType, Status, Provisioning, Termination string
	ID                                                         uint64
	Labels, Metadata                                           map[string]string
	// Disks maps device names to disk names.
	Disks          map[string]string
	ServiceAccount string
	Scopes         []string
	MaxRun         time.Duration
	Network        string
	Subnetwork     string
	ExternalIP     bool
	Created        time.Time
}

// DiskState is what tests see of a disk.
type DiskState struct {
	Name, Zone, Type, SourceImage string
	SizeGB, IOPS, Throughput      int64
	Labels                        map[string]string
	Users                         []string
}

func (e *Emulator) instanceState(in *instance) InstanceState {
	s := InstanceState{Name: in.name, Zone: in.zone, MachineType: in.machineType, Status: in.status, Provisioning: in.provisioning,
		Termination: in.termination, ID: in.id, Labels: map[string]string{}, Metadata: map[string]string{}, Disks: map[string]string{},
		ServiceAccount: in.serviceAccount, Scopes: slices.Clone(in.scopes), MaxRun: in.maxRun, Network: in.network,
		Subnetwork: in.subnetwork, ExternalIP: in.externalIP, Created: in.created}
	for k, v := range in.labels {
		s.Labels[k] = v
	}
	for _, it := range in.metadata {
		s.Metadata[it.key] = it.value
	}
	for _, a := range in.disks {
		s.Disks[a.deviceName] = a.disk.name
	}
	return s
}

// Instance returns an instance's state.
func (e *Emulator) Instance(zone, name string) (InstanceState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick()
	in := e.instances[zone+"/"+name]
	if in == nil {
		return InstanceState{}, false
	}
	return e.instanceState(in), true
}

// Instances returns every instance's state, by zone and name.
func (e *Emulator) Instances() []InstanceState {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick()
	var out []InstanceState
	for _, in := range e.instances {
		out = append(out, e.instanceState(in))
	}
	slices.SortFunc(out, func(a, b InstanceState) int { return strings.Compare(a.Zone+"/"+a.Name, b.Zone+"/"+b.Name) })
	return out
}

// Disks returns every disk's state, by zone and name.
func (e *Emulator) Disks() []DiskState {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []DiskState
	for _, d := range e.disks {
		s := DiskState{Name: d.name, Zone: d.zone, Type: d.typ, SourceImage: d.sourceImage, SizeGB: d.sizeGB, IOPS: d.iops,
			Throughput: d.throughput, Labels: map[string]string{}}
		for k, v := range d.labels {
			s.Labels[k] = v
		}
		for _, u := range d.users {
			s.Users = append(s.Users, u.name)
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b DiskState) int { return strings.Compare(a.Zone+"/"+a.Name, b.Zone+"/"+b.Name) })
	return out
}

// ImageState is what tests see of an image.
type ImageState struct {
	Name, Status, Architecture, SourceDisk string
	SizeGB                                 int64
	Labels                                 map[string]string
	Created                                time.Time
}

// Images returns every image's state, by name.
func (e *Emulator) Images() []ImageState {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []ImageState
	for _, im := range e.images {
		s := ImageState{Name: im.name, Status: im.status, Architecture: im.arch, SourceDisk: last(im.sourceDisk), SizeGB: im.sizeGB,
			Labels: map[string]string{}, Created: im.created}
		for k, v := range im.labels {
			s.Labels[k] = v
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b ImageState) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// DeleteImageNow deletes an image at once, as someone else might.
func (e *Emulator) DeleteImageNow(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.images, name)
}

// AddDisk adds an unattached disk, as one left behind would be.
func (e *Emulator) AddDisk(zone, name string, sizeGB int64, labels map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.disks[zone+"/"+name] = &disk{id: newID(), name: name, zone: zone, typ: "hyperdisk-balanced", sizeGB: sizeGB,
		labels: labels, created: e.clock.Now(), arch: "X86_64"}
}

// MetadataURL is an instance's metadata server, as its guest reaches it.
func (e *Emulator) MetadataURL(zone, name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.instances[zone+"/"+name]
	if in == nil {
		return ""
	}
	return e.machine(in).MetadataURL
}
