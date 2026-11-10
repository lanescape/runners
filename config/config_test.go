package config_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lanescape/runners/config"
)

func TestDefaultConfig(t *testing.T) {
	c, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	if c.Organization.ID != 303994381 {
		t.Errorf("organization ID = %d", c.Organization.ID)
	}
	if want := []string{"northamerica-northeast1-a", "northamerica-northeast1-b"}; !slices.Equal(c.Shapes.Zones, want) {
		t.Errorf("zones = %v, want %v", c.Shapes.Zones, want)
	}
	if c.Shapes.BootDiskGB != 100 || c.Shapes.DiskIOPS != 3000 || c.Shapes.DiskThroughputMiBps != 400 {
		t.Errorf("disks = %+v", c.Shapes)
	}
	big := c.Shapes.Shapes["lanescape-80vcpu-linux-amd64"]
	if big == nil || !slices.Equal(big.MachineTypes, []string{"n4-highcpu-80", "c4-highcpu-96"}) || big.VCPUs != 80 || big.Arch != "amd64" {
		t.Errorf("80-vCPU shape = %+v", big)
	}
	arm := c.Shapes.Shapes["lanescape-16vcpu-linux-arm64"]
	if arm == nil || !slices.Equal(arm.MachineTypes, []string{"c4a-highcpu-16"}) || arm.Arch != "arm64" {
		t.Errorf("arm64 shape = %+v", arm)
	}
	if c.Shapes.Shapes["lanescape-2vcpu-linux-amd64"] == nil {
		t.Error("no 2-vCPU shape, which the specs' runners.json example uses")
	}
	if c.Runner.Version == "" || len(c.Runner.SHA256) != 2 {
		t.Errorf("runner = %+v", c.Runner)
	}
}

// files is a valid configuration that tests change one file of.
func files() fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, name := range []string{"organization.yaml", "shapes.yaml", "runner.yaml"} {
		data, err := os.ReadFile(name)
		if err != nil {
			panic(err)
		}
		fsys[name] = &fstest.MapFile{Data: data}
	}
	return fsys
}

func with(name, data string) fstest.MapFS {
	fsys := files()
	fsys[name] = &fstest.MapFile{Data: []byte(data)}
	return fsys
}

func TestInvalidConfigs(t *testing.T) {
	shapes := func(shape string) string {
		return `
zones: [northamerica-northeast1-a]
boot_disk_gb: 100
disk_iops: 3000
disk_throughput_mibps: 400
shapes:
` + shape
	}
	for _, tc := range []struct {
		name, file, data, want string
	}{
		{"no org ID", "organization.yaml", "id: 0", "numeric ID"},
		{"org by name", "organization.yaml", "id: lanescape", "organization.yaml"},
		{"unknown field", "organization.yaml", "id: 1\nlogin: lanescape", "not found"},
		{"empty file", "runner.yaml", "", "empty"},
		{"bad shape name", "shapes.yaml", shapes("  lanescape-80cpu-linux-amd64: {machine_types: [n4-highcpu-80], memory_gb: 1, work_disk_gb: 1}"), "isn't lanescape-"},
		{"too few vCPUs", "shapes.yaml", shapes("  lanescape-80vcpu-linux-amd64: {machine_types: [n4-highcpu-64], memory_gb: 1, work_disk_gb: 1}"), "fewer than 80"},
		{"wrong architecture", "shapes.yaml", shapes("  lanescape-16vcpu-linux-amd64: {machine_types: [c4a-highcpu-16], memory_gb: 1, work_disk_gb: 1}"), "is arm64, not amd64"},
		{"no machine types", "shapes.yaml", shapes("  lanescape-16vcpu-linux-arm64: {memory_gb: 1, work_disk_gb: 1}"), "no machine types"},
		{"no work disk", "shapes.yaml", shapes("  lanescape-16vcpu-linux-arm64: {machine_types: [c4a-highcpu-16], memory_gb: 32}"), "must be positive"},
		{"duplicate type", "shapes.yaml", shapes("  lanescape-2vcpu-linux-amd64: {machine_types: [n4-highcpu-2, n4-highcpu-2], memory_gb: 4, work_disk_gb: 1}"), "twice"},
		{"no zones", "shapes.yaml", "boot_disk_gb: 100\ndisk_iops: 3000\ndisk_throughput_mibps: 400\nshapes: {lanescape-2vcpu-linux-amd64: {machine_types: [n4-highcpu-2], memory_gb: 4, work_disk_gb: 1}}", "no zones"},
		{"bad runner version", "runner.yaml", "version: v2.338\nsha256: {amd64: " + strings.Repeat("a", 64) + ", arm64: " + strings.Repeat("a", 64) + "}", "isn't a release version"},
		{"missing checksum", "runner.yaml", "version: 2.338.2\nsha256: {amd64: " + strings.Repeat("a", 64) + "}", "sha256 for arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(with(tc.file, tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		fsys := files()
		delete(fsys, "runner.yaml")
		if _, err := config.Load(fsys); err == nil {
			t.Fatal("loaded without runner.yaml")
		}
	})
}
