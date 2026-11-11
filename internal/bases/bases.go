// Package bases makes the service's base images (specs/0003-disks.md, Base
// images): one per architecture, lsr-base-<arch>-<key>, whose key covers
// everything that goes into it. When a deploy changes the key, a pass makes
// a builder, lsr-build-<arch>-<key>, a machine with no account booted from
// Debian's own image, which runs only the service's own script; reads its
// result from its serial console; stops it; and makes the image from its
// boot disk. Until that image is ready, machines boot the previous one.
//
// What builders and images exist is in their names and labels; what can't
// be read back from them is in the bucket:
//
//	bases/<arch>-<key>.json       a key's build: the builders made for it, which of them built it, and its failures
//	logs/<builder>-<created>.txt  a failed builder's serial console
package bases

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
	"github.com/lanescape/runners/machine"
)

// The spec's names, labels and limits.
const (
	ImagePrefix   = "lsr-base-"
	BuilderPrefix = "lsr-build-"

	LabelKind   = "lsr-kind"
	LabelArch   = "lsr-arch"
	LabelKey    = "lsr-key"
	KindBase    = "base"
	KindBuilder = "builder"

	// BootDiskGB is a builder's boot disk, Debian's image's size, and so
	// the base image's.
	BootDiskGB = 10
	// ReportLimit is how long a builder has to say it built the image.
	ReportLimit = 60 * time.Minute
	// BuildLimit is how long a build has, from its builder's making, to
	// give a ready image.
	BuildLimit = 2 * time.Hour
	// MaxRunDuration is when Compute Engine deletes a running builder, if
	// the service hasn't.
	MaxRunDuration = 90 * time.Minute
	// Attempts is how many builders a key gets in all.
	Attempts = 3
	// Kept is how long base images other than the current one, and
	// records of other keys, stay: longer than any pass of a revision of
	// the service a deploy replaced can run.
	Kept = 24 * time.Hour
	// imageWait is as long as a pass waits for an image to be made; one
	// still being made is seen at the next pass.
	imageWait    = 30 * time.Second
	consoleLimit = 4 << 20
	reasonLimit  = 500
)

// The metadata a builder is given.
const (
	MetaStartupScript = "startup-script"
	MetaArch          = "lsr-arch"
	MetaKey           = "lsr-key"
	MetaRunnerVersion = "lsr-runner-version"
	MetaRunnerSHA256  = "lsr-runner-sha256"
	MetaGoVersion     = "lsr-go-version"
	MetaGoSHA256      = "lsr-go-sha256"
	MetaFiles         = "lsr-files"
	metaSerialLogging = "serial-port-logging-enable"
)

// Inputs are what a base image's key covers, in the key's order.
type Inputs struct {
	Arch          string
	Debian        string
	RunnerVersion string
	RunnerSHA256  string
	GoVersion     string
	GoSHA256      string
	Script        string
	// Programs is the digest of the service's programs and the files
	// installed with them (machine.Digest).
	Programs string
}

// Key is a base image's key: the first 20 hex digits of the SHA-256 of
// each input, in order, as its length, an unsigned 64-bit big-endian
// number, and its bytes (specs/0003-disks.md, Base images).
func Key(in Inputs) string {
	h := sha256.New()
	for _, part := range []string{in.Arch, in.Debian, in.RunnerVersion, in.RunnerSHA256, in.GoVersion, in.GoSHA256, in.Script, in.Programs} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

// InputsOf are an architecture's inputs, from the configuration and what's
// embedded in the service's binary.
func InputsOf(cfg *config.Config, arch string) Inputs {
	return Inputs{
		Arch: arch, Debian: cfg.Base.Debian[arch],
		RunnerVersion: cfg.Runner.Version, RunnerSHA256: cfg.Runner.SHA256[arch],
		GoVersion: cfg.Base.Go.Version, GoSHA256: cfg.Base.Go.SHA256[arch],
		Script: machine.BuildScript(), Programs: machine.Digest(),
	}
}

// Keys are each architecture's current key.
func Keys(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, a := range config.Arches {
		out[a] = Key(InputsOf(cfg, a))
	}
	return out
}

// ImageName is a key's base image.
func ImageName(arch, key string) string { return ImagePrefix + arch + "-" + key }

// BuilderName is a key's builder.
func BuilderName(arch, key string) string { return BuilderPrefix + arch + "-" + key }

// requestID is a change's request ID, which the client sends again only
// while it has no answer. Each try of a change is a new request: one that
// failed is never answered again from Compute Engine's memory of it, and
// names, a builder's and an image's, keep a change from being done twice.
func requestID(parts ...string) string {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	return compute.RequestID(append(parts, hex.EncodeToString(nonce[:]))...)
}

func recordName(arch, key string) string { return "bases/" + arch + "-" + key + ".json" }

// Boot is the base image machines of an architecture boot: its image for
// key once it's ready, or else the newest ready base image of that
// architecture, made for an earlier key (specs/0003-disks.md, Which image
// machines boot). With key "", it's the newest.
func Boot(images []compute.Image, arch, key string) (string, bool) {
	var best compute.Image
	for _, im := range images {
		if im.Status != "READY" || !strings.HasPrefix(im.Name, ImagePrefix+arch+"-") {
			continue
		}
		if key != "" && im.Name == ImageName(arch, key) {
			return im.Name, true
		}
		if best.Name == "" || im.Created.After(best.Created) {
			best = im
		}
	}
	return best.Name, best.Name != ""
}

// Record is a key's build, in bases/<arch>-<key>.json.
type Record struct {
	Arch string `json:"arch"`
	Key  string `json:"key"`
	// Attempts is how many builders were made, or may have been: each is
	// counted before it's made.
	Attempts int `json:"attempts"`
	// Started is when the latest attempt was counted, before its builder
	// was made: the build's limit runs from it, whatever becomes of the
	// builder.
	Started time.Time `json:"started,omitzero"`
	// Seen is the instance ID of the builder last seen, so that one that
	// goes without its failure recorded is counted as failed.
	Seen uint64 `json:"seen,omitempty"`
	// Built is the instance ID of the builder that said it built the
	// image, and BuiltAt when the service read it.
	Built   uint64    `json:"built,omitempty"`
	BuiltAt time.Time `json:"built_at,omitzero"`
	// Failures are the builds that failed, one for each builder; Builder
	// is 0 for one never seen.
	Failures []Failure `json:"failures,omitempty"`
}

// Failure is one builder's failed build.
type Failure struct {
	Builder uint64    `json:"builder"`
	At      time.Time `json:"at"`
	Reason  string    `json:"reason"`
}

func (r Record) failed(id uint64) bool {
	return id != 0 && slices.ContainsFunc(r.Failures, func(f Failure) bool { return f.Builder == id })
}

// Service makes the base images.
type Service struct {
	Config  *config.Config
	Compute compute.API
	Objects store.Objects
	Clock   clockwork.Clock
	Logger  *slog.Logger

	// Network and Subnetwork are what a builder joins, and DiskType its
	// boot disk's type: the job machines' own.
	Network    string
	Subnetwork string
	DiskType   string

	// Keys are each architecture's current key; unset, Keys(Config).
	Keys map[string]string
	once sync.Once
	// Script and Files are the builder's script and lsr-files; unset,
	// machine.BuildScript and machine.Archive, in base64.
	Script string
	Files  string
}

func (s *Service) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Service) now() time.Time { return s.Clock.Now() }

func (s *Service) keys() map[string]string {
	s.once.Do(func() {
		if s.Keys == nil {
			s.Keys = Keys(s.Config)
		}
	})
	return s.Keys
}

// Boot is the base image machines of an architecture boot now.
func (s *Service) Boot(images []compute.Image, arch string) (string, bool) {
	return Boot(images, arch, s.keys()[arch])
}

// inventory is what a pass read of builders, images and records.
type inventory struct {
	images   []compute.Image
	builders map[string][]compute.Instance
	listed   bool
	records  map[string]store.Object
}

// AllPools, among what Pass holds back, is every job's: a builder found no
// quota, which machines of every family may share.
const AllPools = "*"

// hold is what a builder that couldn't be made holds back from jobs.
type hold int

const (
	holdNone hold = iota
	// holdFamilies is its machine families', for want of capacity alone.
	holdFamilies
	// holdAll is every job's, for want of quota.
	holdAll
)

// Pass moves each architecture's build on (specs/0003-disks.md, Making a
// base image). Nothing it fails to do stops the rest of the pass. It
// returns the quota pools a builder that couldn't be made holds back from
// queued jobs for the rest of the pass, so that what jobs free builds up for
// it (specs/0001-architecture.md, Passes): its machine families, for a
// stockout, or AllPools, for want of quota.
func (s *Service) Pass(p *pass.Pass) (short []string) {
	ctx := p.Context()
	ims, err := s.Compute.Images(ctx)
	if err != nil {
		s.log().Error("listing images for the base images", "err", err.Error())
		return nil
	}
	inv := &inventory{images: ims, builders: map[string][]compute.Instance{}, listed: true, records: map[string]store.Object{}}
	for _, z := range s.Config.Shapes.Zones {
		ins, err := s.Compute.Instances(ctx, z)
		if err != nil {
			s.log().Error("listing builders", "zone", z, "err", err.Error())
			inv.listed = false
			continue
		}
		for _, in := range ins {
			if strings.HasPrefix(in.Name, BuilderPrefix) && in.Labels[LabelKind] == KindBuilder {
				inv.builders[in.Name] = append(inv.builders[in.Name], in)
			}
		}
	}
	objs, err := s.Objects.List(ctx, "bases/")
	if err != nil {
		s.log().Error("listing base images' records", "err", err.Error())
		return nil
	}
	for _, o := range objs {
		inv.records[o.Name] = o
	}
	keys := s.keys()
	current := map[string]bool{}
	for _, a := range config.Arches {
		current[BuilderName(a, keys[a])] = true
		current[recordName(a, keys[a])] = true
	}
	for _, a := range config.Arches {
		if p.Lost() {
			return short
		}
		switch s.advance(ctx, inv, a, keys[a]) {
		case holdAll:
			if !slices.Contains(short, AllPools) {
				short = append(short, AllPools)
			}
		case holdFamilies:
			for _, mt := range s.Config.Base.Builders[a] {
				if f := mt[:strings.Index(mt, "-")]; !slices.Contains(short, f) {
					short = append(short, f)
				}
			}
		}
	}
	// Another key's builder or record may be a build that a pass of a
	// revision of the service a deploy replaced is still moving on: it
	// goes only once no build can still be going.
	for name, bs := range inv.builders {
		for _, b := range bs {
			if !current[name] && s.now().Sub(b.Created) >= BuildLimit {
				s.deleteBuilder(ctx, b, "it's of another key")
			}
		}
	}
	for name, o := range inv.records {
		if !current[name] && s.now().Sub(o.Updated) >= Kept {
			if err := s.Objects.Delete(ctx, name, o.Generation); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log().Warn("deleting another key's base image record", "record", name, "err", err.Error())
			}
		}
	}
	return short
}

// advance moves one architecture's build for its current key on. It
// reports what a builder that couldn't be made holds back from jobs.
func (s *Service) advance(ctx context.Context, inv *inventory, arch, key string) (h hold) {
	name := ImageName(arch, key)
	builders := inv.builders[BuilderName(arch, key)]
	for _, im := range inv.images {
		if im.Name != name {
			continue
		}
		switch im.Status {
		case "READY":
			for _, b := range builders {
				s.deleteBuilder(ctx, b, "its image is ready")
			}
			if o, ok := inv.records[recordName(arch, key)]; ok {
				if err := s.Objects.Delete(ctx, o.Name, o.Generation); err != nil && !errors.Is(err, store.ErrNotFound) {
					s.log().Warn("deleting a base image's record", "record", o.Name, "err", err.Error())
				}
			}
			s.prune(ctx, inv, arch, im)
		default:
			// Not ready: within the build's limit, from its attempt's
			// start, a failed image is deleted, to be made again from the
			// stopped builder, and one being made or deleted is waited
			// for. Past it, the build fails, whatever becomes of the image
			// or the builder.
			rec, gen, err := s.record(ctx, arch, key)
			if err != nil {
				s.log().Error("reading a base image's record", "arch", arch, "key", key, "err", err.Error())
				return
			}
			since := im.Created
			if !rec.Started.IsZero() && rec.Started.Before(since) {
				since = rec.Started
			}
			for _, b := range builders {
				if b.Created.Before(since) {
					since = b.Created
				}
			}
			late := s.now().Sub(since) >= BuildLimit
			if im.Status == "FAILED" || late && im.Status != "DELETING" {
				s.log().Warn("base image not made; deleting it", "image", name, "status", im.Status, "late", late)
				if err := s.Compute.DeleteImage(ctx, name, requestID("delete unmade base image", name)); err != nil && !errors.Is(err, compute.ErrNotFound) {
					s.log().Warn("deleting a base image that wasn't made", "image", name, "err", err.Error())
				}
			}
			if late {
				reason := fmt.Sprintf("no ready image within %d hours of its builder being made", int(BuildLimit.Hours()))
				for _, b := range builders {
					if rec.failed(b.ID) {
						s.deleteBuilder(ctx, b, "its build failed")
						continue
					}
					rec, gen = s.failWith(ctx, arch, key, rec, gen, b, reason, "")
				}
				if len(builders) == 0 && inv.listed {
					var ok bool
					if rec, gen, ok = s.settle(ctx, arch, key, rec, gen, reason); !ok {
						return
					}
				}
			}
			if len(builders) == 0 && rec.Attempts >= Attempts && len(rec.Failures) >= rec.Attempts {
				s.exhausted(inv, arch, key, rec)
			}
		}
		return
	}

	rec, gen, err := s.record(ctx, arch, key)
	if err != nil {
		s.log().Error("reading a base image's record", "arch", arch, "key", key, "err", err.Error())
		return
	}
	if len(builders) == 0 {
		if !inv.listed {
			// A builder may be in a zone that wasn't listed.
			return
		}
		var ok bool
		if rec, gen, ok = s.settle(ctx, arch, key, rec, gen, ""); !ok {
			return
		}
		if rec.Attempts >= Attempts {
			s.exhausted(inv, arch, key, rec)
			return
		}
		return s.makeBuilder(ctx, arch, key, rec, gen)
	}
	// Two builders, made in two zones when an answer was lost: the oldest
	// goes on.
	slices.SortFunc(builders, func(a, b compute.Instance) int { return a.Created.Compare(b.Created) })
	for _, extra := range builders[1:] {
		s.deleteBuilder(ctx, extra, "another builder of its key was made first")
	}
	b := builders[0]
	if rec.Seen != b.ID && !rec.failed(b.ID) {
		rec.Seen = b.ID
		if gen, err = s.write(ctx, rec, gen); err != nil {
			s.log().Warn("recording a base image builder", "arch", arch, "key", key, "err", err.Error())
			return
		}
	}
	age := s.now().Sub(b.Created)
	// The build's limit runs from its attempt's start.
	since := b.Created
	if !rec.Started.IsZero() && rec.Started.Before(since) {
		since = rec.Started
	}
	switch {
	case rec.failed(b.ID):
		// Its failure is recorded; its deletion didn't finish.
		s.deleteBuilder(ctx, b, "its build failed")
		if rec.Attempts >= Attempts && len(rec.Failures) >= rec.Attempts {
			s.exhausted(inv, arch, key, rec)
		}
	case s.now().Sub(since) >= BuildLimit:
		s.fail(ctx, arch, key, rec, gen, b, fmt.Sprintf("no ready image within %d hours of its builder being made", int(BuildLimit.Hours())))
	case b.Status == "STOPPING":
	case b.Stopped():
		if rec.Built == b.ID {
			s.makeImage(ctx, arch, key, b)
		} else {
			s.fail(ctx, arch, key, rec, gen, b, "the builder stopped before it said it built the image")
		}
	case rec.Built == b.ID:
		s.stopAndMake(ctx, arch, key, b)
	case b.Status != "RUNNING":
		if age >= ReportLimit {
			s.fail(ctx, arch, key, rec, gen, b, fmt.Sprintf("the builder didn't start within %d minutes", int(ReportLimit.Minutes())))
		}
	default:
		console, err := s.Compute.SerialOutput(ctx, b.Zone, b.Name, consoleLimit)
		if err != nil {
			// Its silence is judged only from a console read; the build's
			// limit, and Compute Engine's, bound one never read.
			s.log().Warn("reading a builder's console", "builder", b.Name, "err", err.Error())
			return
		}
		built, failed := Result(console, arch, key)
		switch {
		case failed != "":
			s.failWith(ctx, arch, key, rec, gen, b, "the builder failed: "+failed, console)
		case built:
			rec.Built, rec.BuiltAt = b.ID, s.now()
			if _, err := s.write(ctx, rec, gen); err != nil {
				s.log().Warn("recording a built base image", "arch", arch, "key", key, "err", err.Error())
				return
			}
			s.log().Info("base image built", "arch", arch, "key", key, "builder", b.Name, "instance_id", b.ID)
			s.stopAndMake(ctx, arch, key, b)
		case age >= ReportLimit:
			s.failWith(ctx, arch, key, rec, gen, b, fmt.Sprintf("the builder didn't say it built the image within %d minutes", int(ReportLimit.Minutes())), console)
		}
	}
	return holdNone
}

// settle records every attempt whose builder went without its failure
// recorded as failed: the builder last seen, such as one deleted by someone
// else or by Compute Engine at its maximum run duration while the service
// couldn't act, and any other counted and never seen to fail, such as one
// that went before a pass saw it, or whose making's answer was lost and
// that was never made. reason, if given, is why. It reports whether the
// record is as written.
func (s *Service) settle(ctx context.Context, arch, key string, rec Record, gen int64, reason string) (Record, int64, bool) {
	changed := false
	if rec.Seen != 0 && !rec.failed(rec.Seen) {
		why := reason
		if why == "" {
			why = "the builder went before its image was ready"
		}
		rec.Failures = append(rec.Failures, Failure{Builder: rec.Seen, At: s.now(), Reason: why})
		changed = true
	}
	for len(rec.Failures) < rec.Attempts {
		why := reason
		if why == "" {
			why = "the builder went before a pass saw it, or was never made"
		}
		rec.Failures = append(rec.Failures, Failure{At: s.now(), Reason: why})
		changed = true
	}
	if !changed {
		return rec, gen, true
	}
	gen, err := s.write(ctx, rec, gen)
	if err != nil {
		s.log().Warn("recording a base image builder that went", "arch", arch, "key", key, "err", err.Error())
		return rec, gen, false
	}
	s.log().Warn("base image build failed", "arch", arch, "key", key, "attempt", rec.Attempts, "of", Attempts,
		"reason", rec.Failures[len(rec.Failures)-1].Reason)
	return rec, gen, true
}

// exhausted logs, at every pass, that a key's builders have all failed.
func (s *Service) exhausted(inv *inventory, arch, key string, rec Record) {
	last := ""
	if n := len(rec.Failures); n > 0 {
		last = rec.Failures[n-1].Reason
	}
	boot, _ := Boot(inv.images, arch, key)
	s.log().Error("base image builds failed; making no more for this key", "arch", arch, "key", key, "image", ImageName(arch, key),
		"attempts", rec.Attempts, "last_failure", last, "machines_boot", boot)
}

// Result reads a builder's console: whether it said it built the image of
// arch and key, or why it failed, which wins if it said both.
func Result(console, arch, key string) (built bool, failed string) {
	want := machine.Built + arch + "-" + key
	for line := range strings.SplitSeq(console, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, strings.TrimSpace(machine.Failed)); i >= 0 {
			failed = clean(line[i+len(strings.TrimSpace(machine.Failed)):])
			if failed == "" {
				failed = "no reason given"
			}
		}
		if strings.HasSuffix(line, want) {
			built = true
		}
	}
	return built, failed
}

// clean keeps a reason printable and short.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > reasonLimit {
		s = s[:reasonLimit] + "..."
	}
	return s
}

// makeBuilder counts an attempt, then makes the key's builder in the first
// placement with capacity, or takes the attempt back if none had any, and
// reports what it then holds back from jobs.
func (s *Service) makeBuilder(ctx context.Context, arch, key string, rec Record, gen int64) (h hold) {
	files := s.Files
	if files == "" {
		b, err := machine.Archive()
		if err != nil {
			s.log().Error("making a builder's files", "err", err.Error())
			return
		}
		files = base64.StdEncoding.EncodeToString(b)
	}
	script := s.Script
	if script == "" {
		script = machine.BuildScript()
	}
	rec.Attempts++
	rec.Started = s.now()
	attempt := rec.Attempts
	gen, err := s.write(ctx, rec, gen)
	if err != nil {
		s.log().Warn("counting a base image's builder", "arch", arch, "key", key, "err", err.Error())
		return
	}
	name := BuilderName(arch, key)
	cfg := s.Config
	spec := compute.InstanceSpec{
		Name: name, Image: cfg.Base.Debian[arch], ImageProject: config.DebianProject, BootDiskGB: BootDiskGB, DiskType: s.DiskType,
		Labels: map[string]string{LabelKind: KindBuilder, LabelArch: arch, LabelKey: key},
		Metadata: map[string]string{
			MetaStartupScript: script, MetaArch: arch, MetaKey: key,
			MetaRunnerVersion: cfg.Runner.Version, MetaRunnerSHA256: cfg.Runner.SHA256[arch],
			MetaGoVersion: cfg.Base.Go.Version, MetaGoSHA256: cfg.Base.Go.SHA256[arch],
			MetaFiles: files, metaSerialLogging: "TRUE",
		},
		Network: s.Network, Subnetwork: s.Subnetwork, MaxRunDuration: MaxRunDuration, DeleteAtMaxRun: true,
	}
	var last error
	quota := false
	for _, mt := range cfg.Base.Builders[arch] {
		for _, z := range cfg.Shapes.Zones {
			spec.MachineType = mt
			err := s.Compute.CreateInstance(ctx, z, spec, requestID("base image builder", name, z, mt))
			if err == nil || errors.Is(err, compute.ErrExists) {
				s.log().Info("base image builder made", "arch", arch, "key", key, "builder", name, "zone", z, "machine_type", mt, "attempt", attempt)
				return
			}
			if compute.IsUnavailable(err) {
				// It may have been made: the next pass sees.
				s.log().Warn("making a base image builder", "arch", arch, "key", key, "zone", z, "machine_type", mt, "err", err.Error())
				return
			}
			last = err
			if capacity := (*compute.CapacityError)(nil); errors.As(err, &capacity) {
				quota = quota || !capacity.Stockout
				continue
			}
			s.log().Error("making a base image builder", "arch", arch, "key", key, "zone", z, "machine_type", mt, "err", err.Error())
			return
		}
	}
	// No placement had capacity: no builder was made, so the attempt is
	// taken back, and the next pass tries again.
	h = holdFamilies
	if quota {
		h = holdAll
	}
	s.log().Info("no capacity or quota for a base image builder; jobs are held back this pass", "arch", arch, "key", key,
		"every_job", quota, "err", fmt.Sprint(last))
	rec.Attempts--
	if _, err := s.write(ctx, rec, gen); err != nil {
		s.log().Warn("taking back a base image builder's attempt", "arch", arch, "key", key, "err", err.Error())
	}
	return h
}

// stopAndMake stops a builder that built its image, then makes the image.
func (s *Service) stopAndMake(ctx context.Context, arch, key string, b compute.Instance) {
	if err := s.Compute.StopInstance(ctx, b.Zone, b.Name, requestID("stop base image builder", b.Name)); err != nil {
		s.log().Warn("stopping a base image builder", "builder", b.Name, "err", err.Error())
		return
	}
	got, err := s.Compute.Instance(ctx, b.Zone, b.Name)
	if err != nil || got.ID != b.ID || got.Status != "TERMINATED" && got.Status != "STOPPED" {
		return
	}
	s.makeImage(ctx, arch, key, got)
}

// makeImage makes the base image from a stopped builder's boot disk. A pass
// waits only a short while for it; the next sees it ready.
func (s *Service) makeImage(ctx context.Context, arch, key string, b compute.Instance) {
	disk := b.Name
	for _, d := range b.Disks {
		if d.Boot {
			disk = d.Disk
		}
	}
	name := ImageName(arch, key)
	ictx, cancel := context.WithTimeout(ctx, imageWait)
	defer cancel()
	err := s.Compute.CreateImage(ictx, compute.ImageSpec{Name: name, SourceZone: b.Zone, SourceDisk: disk,
		Labels: map[string]string{LabelKind: KindBase, LabelArch: arch, LabelKey: key}},
		requestID("base image", name))
	switch {
	case err == nil:
		s.log().Info("base image made", "image", name, "arch", arch, "key", key, "builder", b.Name)
	case errors.Is(err, compute.ErrExists):
	case compute.IsUnavailable(err):
		s.log().Info("base image still being made", "image", name, "err", err.Error())
	default:
		s.log().Warn("making a base image", "image", name, "err", err.Error())
	}
}

// fail ends a builder's build, saying why, with its console if Compute
// Engine still serves it.
func (s *Service) fail(ctx context.Context, arch, key string, rec Record, gen int64, b compute.Instance, reason string) {
	console := ""
	if b.Status == "RUNNING" {
		console, _ = s.Compute.SerialOutput(ctx, b.Zone, b.Name, consoleLimit)
	}
	s.failWith(ctx, arch, key, rec, gen, b, reason, console)
}

// failWith records a builder's failure, once, saves its console, and deletes
// it. Nothing waits on the console. It returns the record as written.
func (s *Service) failWith(ctx context.Context, arch, key string, rec Record, gen int64, b compute.Instance, reason, console string) (Record, int64) {
	if console != "" {
		logName := fmt.Sprintf("logs/%s-%d.txt", b.Name, b.Created.Unix())
		if _, err := s.Objects.Create(ctx, logName, []byte(console)); err != nil && !errors.Is(err, store.ErrPrecondition) {
			s.log().Warn("saving a failed builder's console", "builder", b.Name, "err", err.Error())
		}
	}
	if !rec.failed(b.ID) {
		rec.Failures = append(rec.Failures, Failure{Builder: b.ID, At: s.now(), Reason: reason})
		next, err := s.write(ctx, rec, gen)
		if err != nil {
			s.log().Warn("recording a failed base image build", "arch", arch, "key", key, "err", err.Error())
			return rec, gen
		}
		gen = next
	}
	s.log().Warn("base image build failed", "arch", arch, "key", key, "builder", b.Name, "instance_id", b.ID,
		"attempt", rec.Attempts, "of", Attempts, "reason", reason)
	s.deleteBuilder(ctx, b, "its build failed")
	return rec, gen
}

func (s *Service) deleteBuilder(ctx context.Context, b compute.Instance, why string) {
	err := s.Compute.DeleteInstance(ctx, b.Zone, b.Name, requestID("delete base image builder", b.Name))
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		s.log().Warn("deleting a base image builder", "builder", b.Name, "err", err.Error())
		return
	}
	s.log().Info("base image builder deleted", "builder", b.Name, "instance_id", b.ID, "why", why)
}

// prune deletes an architecture's other base images once both they and the
// current one are more than a day old.
func (s *Service) prune(ctx context.Context, inv *inventory, arch string, cur compute.Image) {
	if s.now().Sub(cur.Created) < Kept {
		return
	}
	for _, im := range inv.images {
		if im.Name == cur.Name || !strings.HasPrefix(im.Name, ImagePrefix+arch+"-") || im.Status == "DELETING" || s.now().Sub(im.Created) < Kept {
			continue
		}
		err := s.Compute.DeleteImage(ctx, im.Name, requestID("delete base image", im.Name))
		if err != nil && !errors.Is(err, compute.ErrNotFound) {
			s.log().Warn("deleting an old base image", "image", im.Name, "err", err.Error())
			continue
		}
		s.log().Info("old base image deleted", "image", im.Name, "current", cur.Name)
	}
}

// record reads a key's record, or a new one, with its generation, 0 if it
// doesn't exist yet.
func (s *Service) record(ctx context.Context, arch, key string) (Record, int64, error) {
	data, gen, err := s.Objects.Get(ctx, recordName(arch, key))
	if errors.Is(err, store.ErrNotFound) {
		return Record{Arch: arch, Key: key}, 0, nil
	}
	if err != nil {
		return Record{}, 0, err
	}
	var rec Record
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&rec); err != nil {
		return Record{}, 0, fmt.Errorf("record %s: %w", recordName(arch, key), err)
	}
	return rec, gen, nil
}

// write writes a key's record at the generation it was read at.
func (s *Service) write(ctx context.Context, rec Record, gen int64) (int64, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	if gen == 0 {
		return s.Objects.Create(ctx, recordName(rec.Arch, rec.Key), data)
	}
	return s.Objects.Replace(ctx, recordName(rec.Arch, rec.Key), data, gen)
}
