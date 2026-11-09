// Package config is the service's configuration: the organization's ID,
// shapes and the runner version. The YAML files beside this one are
// embedded into the service's binary, and validated when it starts.
package config

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"
)

//go:embed *.yaml
var files embed.FS

// Config is the whole configuration.
type Config struct {
	Organization Organization
	Shapes       Shapes
	Runner       Runner
}

// Organization is the one organization the service serves.
type Organization struct {
	ID int64 `yaml:"id"`
}

// Shapes are the machines jobs may ask for.
type Shapes struct {
	Zones               []string          `yaml:"zones"`
	BootDiskGB          int               `yaml:"boot_disk_gb"`
	DiskIOPS            int               `yaml:"disk_iops"`
	DiskThroughputMiBps int               `yaml:"disk_throughput_mibps"`
	Shapes              map[string]*Shape `yaml:"shapes"`
}

// Shape is one shape. Name, VCPUs and Arch come from the shape's name.
type Shape struct {
	Name         string   `yaml:"-"`
	VCPUs        int      `yaml:"-"`
	Arch         string   `yaml:"-"`
	MachineTypes []string `yaml:"machine_types"`
	MemoryGB     int      `yaml:"memory_gb"`
	WorkDiskGB   int      `yaml:"work_disk_gb"`
}

// Runner is the GitHub runner release the base images install.
type Runner struct {
	Version string            `yaml:"version"`
	SHA256  map[string]string `yaml:"sha256"`
}

// Arches are the architectures machines have.
var Arches = []string{"amd64", "arm64"}

// Default returns the embedded configuration, validated.
func Default() (*Config, error) { return Load(files) }

// Load reads and validates the configuration in fsys.
func Load(fsys fs.FS) (*Config, error) {
	var c Config
	for _, f := range []struct {
		name string
		v    any
	}{
		{"organization.yaml", &c.Organization},
		{"shapes.yaml", &c.Shapes},
		{"runner.yaml", &c.Runner},
	} {
		if err := decode(fsys, f.name, f.v); err != nil {
			return nil, err
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func decode(fsys fs.FS, name string, v any) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("config: %s is empty", name)
		}
		return fmt.Errorf("config: %s: %w", name, err)
	}
	return nil
}

var (
	shapeName   = regexp.MustCompile(`^lanescape-([1-9][0-9]*)vcpu-linux-(amd64|arm64)$`)
	machineType = regexp.MustCompile(`^([a-z][a-z0-9]*)-[a-z]+-([1-9][0-9]*)$`)
	zoneName    = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
	version     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// armFamilies are Compute Engine's Arm machine families; every other family
// is x86.
var armFamilies = map[string]bool{"c4a": true, "n4a": true, "t2a": true}

// Validate checks the configuration as a whole.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("config: "+format, args...)) }

	if c.Organization.ID <= 0 {
		bad("organization.yaml: id must be the organization's numeric ID")
	}

	s := &c.Shapes
	if len(s.Zones) == 0 {
		bad("shapes.yaml: no zones")
	}
	seenZone := map[string]bool{}
	for _, z := range s.Zones {
		if !zoneName.MatchString(z) {
			bad("shapes.yaml: zone %q isn't a zone name", z)
		}
		if seenZone[z] {
			bad("shapes.yaml: zone %q listed twice", z)
		}
		seenZone[z] = true
	}
	if s.BootDiskGB <= 0 || s.DiskIOPS <= 0 || s.DiskThroughputMiBps <= 0 {
		bad("shapes.yaml: boot_disk_gb, disk_iops and disk_throughput_mibps must be positive")
	}
	if len(s.Shapes) == 0 {
		bad("shapes.yaml: no shapes")
	}
	for name, sh := range s.Shapes {
		if sh == nil {
			bad("shapes.yaml: %s is empty", name)
			continue
		}
		m := shapeName.FindStringSubmatch(name)
		if m == nil {
			bad("shapes.yaml: %q isn't lanescape-<vCPUs>vcpu-linux-<amd64|arm64>", name)
			continue
		}
		sh.Name, sh.Arch = name, m[2]
		sh.VCPUs, _ = strconv.Atoi(m[1])
		if len(sh.MachineTypes) == 0 {
			bad("shapes.yaml: %s has no machine types", name)
		}
		seenType := map[string]bool{}
		for _, mt := range sh.MachineTypes {
			mm := machineType.FindStringSubmatch(mt)
			if mm == nil {
				bad("shapes.yaml: %s: %q isn't a predefined machine type", name, mt)
				continue
			}
			if seenType[mt] {
				bad("shapes.yaml: %s lists %s twice", name, mt)
			}
			seenType[mt] = true
			if n, _ := strconv.Atoi(mm[2]); n < sh.VCPUs {
				bad("shapes.yaml: %s: %s has fewer than %d vCPUs", name, mt, sh.VCPUs)
			}
			if arch := archOf(mm[1]); arch != sh.Arch {
				bad("shapes.yaml: %s: %s is %s, not %s", name, mt, arch, sh.Arch)
			}
		}
		if sh.MemoryGB <= 0 || sh.WorkDiskGB <= 0 {
			bad("shapes.yaml: %s: memory_gb and work_disk_gb must be positive", name)
		}
	}

	if !version.MatchString(c.Runner.Version) {
		bad("runner.yaml: version %q isn't a release version such as 2.330.0", c.Runner.Version)
	}
	for _, a := range Arches {
		if !sha256Hex.MatchString(c.Runner.SHA256[a]) {
			bad("runner.yaml: sha256 for %s must be 64 lowercase hex digits", a)
		}
	}
	for a := range c.Runner.SHA256 {
		if a != "amd64" && a != "arm64" {
			bad("runner.yaml: sha256 for unknown architecture %q", a)
		}
	}
	return errors.Join(errs...)
}

func archOf(family string) string {
	if armFamilies[family] {
		return "arm64"
	}
	return "amd64"
}
