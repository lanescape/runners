package compute

import (
	"fmt"
	"net/http"

	gce "google.golang.org/api/compute/v1"
)

// The sizes a Hyperdisk Balanced disk may be, as its disk type reports them.
const (
	HyperdiskBalancedMinGB = 4
	HyperdiskBalancedMaxGB = 65536
)

// MaxDisksSizeGB is the most every emulated machine type's disks may hold
// in all, unless SetMaxDisksSize says otherwise.
const MaxDisksSizeGB = 263168

// HyperdiskBalancedRefusal is why Compute Engine would refuse a Hyperdisk
// Balanced disk of sizeGB with this provisioned performance, or "", as
// Google documents its limits: IOPS from 3,000, or all a smaller disk
// allows, to 500 for each GB of size, and at most 160,000; throughput from 140 MiB/s, and one for every 256 IOPS,
// to 2,400 MiB/s, and one for every 4 IOPS, each rounded down. A value of 0
// is one the request leaves to Compute Engine, which gives its default.
func HyperdiskBalancedRefusal(sizeGB, iops, throughput int64) string {
	if sizeGB < HyperdiskBalancedMinGB || sizeGB > HyperdiskBalancedMaxGB {
		return fmt.Sprintf("Invalid value for field 'resource.sizeGb': '%d'. Disk size must be between %d GB and %d GB.",
			sizeGB, HyperdiskBalancedMinGB, HyperdiskBalancedMaxGB)
	}
	if iops != 0 {
		// A disk too small for 3,000 IOPS may have only what its size allows.
		hi := min(500*sizeGB, 160000)
		if lo := min(3000, hi); iops < lo || iops > hi {
			return fmt.Sprintf("Invalid value for field 'resource.provisionedIops': '%d'. Requested provisioned IOPS must be between %d and %d for a %d GB disk.",
				iops, lo, hi, sizeGB)
		}
	}
	if throughput != 0 {
		base := iops
		if base == 0 {
			base = DefaultHyperdiskBalancedIOPS(sizeGB)
		}
		if lo, hi := max(140, base/256), min(2400, base/4); throughput < lo || throughput > hi {
			return fmt.Sprintf("Invalid value for field 'resource.provisionedThroughput': '%d'. Requested provisioned throughput must be between %d and %d MiB/s for %d IOPS.",
				throughput, lo, hi, base)
		}
	}
	return ""
}

// DefaultHyperdiskBalancedIOPS is the IOPS Compute Engine gives a
// Hyperdisk Balanced disk of sizeGB whose request leaves them out, as Google
// documents it: 3,000 and 6 more for each GB, or all a smaller disk allows.
func DefaultHyperdiskBalancedIOPS(sizeGB int64) int64 {
	return min(3000+6*sizeGB, 500*sizeGB, 160000)
}

// SetMaxDisksSize sets the most a machine type's disks may hold in all, as
// its machine type reports it and attaching a disk insists on.
func (e *Emulator) SetMaxDisksSize(machineType string, gb int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.maxDisksSize[machineType] = gb
}

// maxDisksSizeOf is a machine type's maximum disks size, held under e.mu.
func (e *Emulator) maxDisksSizeOf(machineType string) int64 {
	if gb, ok := e.maxDisksSize[machineType]; ok {
		return gb
	}
	return MaxDisksSizeGB
}

func (e *Emulator) getMachineType(w http.ResponseWriter, r *http.Request, zone string) {
	name := r.PathValue("name")
	family, vcpus, ok := machineShape(name)
	if !ok {
		writeError(w, notFound("projects/"+e.Project+"/zones/"+zone+"/machineTypes/"+name))
		return
	}
	writeJSON(w, http.StatusOK, &gce.MachineType{Kind: "compute#machineType", Name: name, Zone: zone, GuestCpus: vcpus,
		Architecture: archOf(family), MaximumPersistentDisks: 128, MaximumPersistentDisksSizeGb: e.maxDisksSizeOf(name),
		SelfLink: e.zoneURL(zone) + "/machineTypes/" + name})
}

func (e *Emulator) getDiskType(w http.ResponseWriter, r *http.Request, zone string) {
	name := r.PathValue("name")
	out := &gce.DiskType{Kind: "compute#diskType", Name: name, Zone: e.zoneURL(zone), SelfLink: e.zoneURL(zone) + "/diskTypes/" + name}
	switch name {
	case "hyperdisk-balanced":
		out.ValidDiskSize, out.DefaultDiskSizeGb = fmt.Sprintf("%dGB-%dGB", HyperdiskBalancedMinGB, HyperdiskBalancedMaxGB), 100
	case "hyperdisk-throughput", "hyperdisk-extreme", "pd-balanced", "pd-ssd", "pd-standard":
		out.ValidDiskSize, out.DefaultDiskSizeGb = "10GB-65536GB", 100
	default:
		writeError(w, notFound("projects/"+e.Project+"/zones/"+zone+"/diskTypes/"+name))
		return
	}
	writeJSON(w, http.StatusOK, out)
}
