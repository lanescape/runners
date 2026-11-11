// Package compute is the service's view of Compute Engine: its machines,
// their disks and the images they boot, in one project, through Google's
// REST client. Every change carries a request ID, retried with the same ID
// when no answer came, so Compute Engine does it once however often it's
// asked; and every failure is classed, so that only confirmed answers decide
// anything.
package compute

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gce "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// ErrNotFound is Compute Engine's confirmed answer that something doesn't
// exist.
var ErrNotFound = errors.New("not found in Compute Engine")

// ErrExists means something of that name exists already.
var ErrExists = errors.New("already exists in Compute Engine")

// ErrInUse means a disk is attached to an instance.
var ErrInUse = errors.New("in use in Compute Engine")

// ErrFingerprint means a change named a fingerprint, such as a resource's
// labels', that's no longer the resource's.
var ErrFingerprint = errors.New("fingerprint no longer current in Compute Engine")

// CapacityError means Compute Engine couldn't make a machine where it was
// asked for: the zone has no capacity for it (a stockout), or the project's
// quota is used up. Another machine type or zone may succeed.
type CapacityError struct {
	Stockout bool
	Message  string
}

func (c *CapacityError) Error() string {
	if c.Stockout {
		return "no capacity: " + c.Message
	}
	return "quota exceeded: " + c.Message
}

// IsCapacity reports whether err is a CapacityError.
func IsCapacity(err error) bool {
	var c *CapacityError
	return errors.As(err, &c)
}

// Failed is any other confirmed refusal or failure of a request.
type Failed struct{ Err error }

func (f *Failed) Error() string { return "Compute Engine refused: " + f.Err.Error() }
func (f *Failed) Unwrap() error { return f.Err }

// Unavailable wraps every failure that confirms nothing: Compute Engine
// couldn't be reached, timed out, or an operation's outcome is unknown.
type Unavailable struct{ Err error }

func (u *Unavailable) Error() string { return "Compute Engine unavailable: " + u.Err.Error() }
func (u *Unavailable) Unwrap() error { return u.Err }

// IsUnavailable reports whether err is an Unavailable.
func IsUnavailable(err error) bool {
	var u *Unavailable
	return errors.As(err, &u)
}

// AttachedDisk is a disk attached to an instance.
type AttachedDisk struct {
	DeviceName string
	Disk       string
	Boot       bool
	AutoDelete bool
}

// Instance is a machine, as Compute Engine reports it.
type Instance struct {
	Name        string
	ID          uint64
	Zone        string
	MachineType string
	Status      string
	Spot        bool
	Created     time.Time
	// LastStop is when Compute Engine last stopped it, for whatever
	// reason, zero if it never has.
	LastStop time.Time
	Labels   map[string]string
	// Metadata holds the instance's metadata, which can include
	// credentials: never log it.
	Metadata map[string]string
	Disks    []AttachedDisk
}

// Stopped reports whether the machine's guest is no longer running, or
// soon won't be.
func (i Instance) Stopped() bool {
	switch i.Status {
	case "STOPPING", "STOPPED", "SUSPENDING", "SUSPENDED", "TERMINATED":
		return true
	}
	return false
}

// Disk is a persistent disk.
type Disk struct {
	Name    string
	ID      uint64
	Zone    string
	SizeGB  int64
	Status  string
	Labels  map[string]string
	Users   []string // the instances it's attached to, by name
	Created time.Time
}

// Image is an image in the project.
type Image struct {
	Name         string
	ID           uint64
	Family       string
	Status       string
	Architecture string
	// DiskSizeGB is the size of the disk it was made from, the least a
	// disk made from it may be.
	DiskSizeGB int64
	// SourceDisk is the disk it was made from, by name.
	SourceDisk       string
	Labels           map[string]string
	LabelFingerprint string
	Created          time.Time
}

// ImageSpec is an image to make from a disk.
type ImageSpec struct {
	Name string
	// SourceZone and SourceDisk are the disk it's made from, which must be
	// attached to no running instance.
	SourceZone string
	SourceDisk string
	Labels     map[string]string
}

// InstanceSpec is a machine to make.
type InstanceSpec struct {
	Name        string
	MachineType string
	Spot        bool
	// Image is the boot image's name, in the project, or in ImageProject
	// if that's set, such as Debian's own images' project.
	Image                 string
	ImageProject          string
	BootDiskGB            int64
	DiskType              string
	ProvisionedIOPS       int64
	ProvisionedThroughput int64
	Labels                map[string]string
	// Metadata can include credentials: never log it.
	Metadata map[string]string
	// ServiceAccount is the machine's account, with Scopes; with none, the
	// machine has no account, and so no identity at all.
	ServiceAccount string
	Scopes         []string
	Network        string
	// Subnetwork is the subnetwork the machine joins, in its zone's
	// region.
	Subnetwork string
	// MaxRunDuration is when Compute Engine itself stops the machine, or
	// deletes it if DeleteAtMaxRun.
	MaxRunDuration time.Duration
	DeleteAtMaxRun bool
}

// DiskSpec is a disk to make.
type DiskSpec struct {
	Name                  string
	SizeGB                int64
	DiskType              string
	ProvisionedIOPS       int64
	ProvisionedThroughput int64
	Labels                map[string]string
	// SourceImage, if set, is the image the disk starts from.
	SourceImage string
}

// API is what the service does in Compute Engine. Every change takes a
// request ID, which RequestID makes from what identifies the change, and
// waits for its operation to finish.
type API interface {
	Instances(ctx context.Context, zone string) ([]Instance, error)
	Instance(ctx context.Context, zone, name string) (Instance, error)
	CreateInstance(ctx context.Context, zone string, spec InstanceSpec, requestID string) error
	StopInstance(ctx context.Context, zone, name, requestID string) error
	DeleteInstance(ctx context.Context, zone, name, requestID string) error
	AttachDisk(ctx context.Context, zone, instance, disk, deviceName, requestID string) error
	Disks(ctx context.Context, zone string) ([]Disk, error)
	Disk(ctx context.Context, zone, name string) (Disk, error)
	CreateDisk(ctx context.Context, zone string, spec DiskSpec, requestID string) error
	DeleteDisk(ctx context.Context, zone, name, requestID string) error
	Images(ctx context.Context) ([]Image, error)
	Image(ctx context.Context, name string) (Image, error)
	CreateImage(ctx context.Context, spec ImageSpec, requestID string) error
	DeleteImage(ctx context.Context, name, requestID string) error
	// SetImageLabels replaces an image's labels, if they're still those
	// fingerprint names.
	SetImageLabels(ctx context.Context, name string, labels map[string]string, fingerprint string) error
	// SerialOutput is an instance's serial console output, up to limit
	// bytes from its start.
	SerialOutput(ctx context.Context, zone, name string, limit int) (string, error)
	// Preempted lists the numeric IDs of instances Compute Engine records
	// preempting in a zone, from its zone operations, which it keeps for as
	// little as an hour.
	Preempted(ctx context.Context, zone string) ([]uint64, error)
}

// RequestID makes a request ID from what identifies a change, so that the
// same change always carries the same ID: a version 4 UUID in form, as
// Compute Engine requires.
func RequestID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// Client is API over Compute Engine's REST API.
type Client struct {
	svc     *gce.Service
	project string
	// Retries is how many times a change is sent again, with the same
	// request ID, when no answer came.
	Retries int
}

// New connects to Compute Engine for project. opts are passed to the
// client, such as an emulator's endpoint in tests.
func New(ctx context.Context, project string, opts ...option.ClientOption) (*Client, error) {
	svc, err := gce.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("compute engine client: %w", err)
	}
	return &Client{svc: svc, project: project, Retries: 3}, nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return &Unavailable{Err: err}
	}
	reason := ""
	if len(ge.Errors) > 0 {
		reason = ge.Errors[0].Reason
	}
	switch {
	case ge.Code == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, ge.Message)
	case ge.Code == http.StatusConflict && reason == "alreadyExists":
		return fmt.Errorf("%w: %s", ErrExists, ge.Message)
	case reason == "quotaExceeded" || reason == "QUOTA_EXCEEDED":
		return &CapacityError{Message: ge.Message}
	case stockout(reason):
		// Compute Engine can refuse a request for want of capacity at
		// once, as well as in its operation.
		return &CapacityError{Stockout: true, Message: ge.Message}
	case reason == "resourceInUseByAnotherResource":
		return fmt.Errorf("%w: %s", ErrInUse, ge.Message)
	case ge.Code == http.StatusPreconditionFailed || reason == "conditionNotMet":
		return fmt.Errorf("%w: %s", ErrFingerprint, ge.Message)
	case ge.Code == http.StatusTooManyRequests || ge.Code >= 500 || reason == "rateLimitExceeded" || reason == "resourceNotReady":
		return &Unavailable{Err: err}
	case ge.Code >= 400 && ge.Code < 500:
		return &Failed{Err: err}
	}
	return &Unavailable{Err: err}
}

// stockout reports whether an error's reason or code says the zone has no
// capacity for what was asked.
func stockout(code string) bool {
	return strings.HasPrefix(code, "ZONE_RESOURCE_POOL_EXHAUSTED") || code == "RESOURCE_POOL_EXHAUSTED" ||
		code == "RESOURCE_EXHAUSTED" || code == "STOCKOUT" || code == "resourcePoolExhausted" ||
		code == "insufficientCapacity" || code == "zoneResourcePoolExhausted"
}

// opError classes an operation's error, as Compute Engine reports what it
// couldn't do after accepting a request.
func opError(op *gce.Operation) error {
	if op.Error == nil || len(op.Error.Errors) == 0 {
		return nil
	}
	e := op.Error.Errors[0]
	switch {
	case stockout(e.Code):
		return &CapacityError{Stockout: true, Message: e.Message}
	case e.Code == "QUOTA_EXCEEDED":
		return &CapacityError{Message: e.Message}
	case e.Code == "RESOURCE_ALREADY_EXISTS":
		return fmt.Errorf("%w: %s", ErrExists, e.Message)
	case e.Code == "RESOURCE_NOT_FOUND":
		return fmt.Errorf("%w: %s", ErrNotFound, e.Message)
	case e.Code == "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE":
		return fmt.Errorf("%w: %s", ErrInUse, e.Message)
	}
	// A finished operation's error is its outcome, even a passing one such
	// as an internal error: the request ID has had its answer, and another
	// try needs another ID.
	return &Failed{Err: fmt.Errorf("%s: %s", e.Code, e.Message)}
}

// change sends a change, again with the same request ID if no answer came,
// and waits for its operation.
func (c *Client) change(ctx context.Context, zone string, send func() (*gce.Operation, error)) error {
	var op *gce.Operation
	var err error
	for attempt := 0; ; attempt++ {
		op, err = send()
		err = classify(err)
		if err == nil || !IsUnavailable(err) || attempt >= c.Retries || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	return c.wait(ctx, zone, op)
}

// wait waits for an operation to finish, as long as ctx allows.
func (c *Client) wait(ctx context.Context, zone string, op *gce.Operation) error {
	for op.Status != "DONE" {
		var next *gce.Operation
		var err error
		if zone == "" {
			next, err = c.svc.GlobalOperations.Wait(c.project, op.Name).Context(ctx).Do()
		} else {
			next, err = c.svc.ZoneOperations.Wait(c.project, zone, op.Name).Context(ctx).Do()
		}
		if err != nil {
			if ctx.Err() != nil {
				return &Unavailable{Err: fmt.Errorf("operation %s didn't finish in time: %w", op.Name, ctx.Err())}
			}
			if err := classify(err); !IsUnavailable(err) {
				return &Unavailable{Err: fmt.Errorf("operation %s: %w", op.Name, err)}
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		op = next
	}
	return opError(op)
}

func last(s string) string { return s[strings.LastIndex(s, "/")+1:] }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func instanceOf(i *gce.Instance) Instance {
	out := Instance{Name: i.Name, ID: i.Id, Zone: last(i.Zone), MachineType: last(i.MachineType), Status: i.Status,
		Created: parseTime(i.CreationTimestamp), LastStop: parseTime(i.LastStopTimestamp), Labels: i.Labels, Metadata: map[string]string{}}
	if i.Scheduling != nil {
		out.Spot = i.Scheduling.ProvisioningModel == "SPOT"
	}
	if i.Metadata != nil {
		for _, it := range i.Metadata.Items {
			if it.Value != nil {
				out.Metadata[it.Key] = *it.Value
			}
		}
	}
	for _, d := range i.Disks {
		out.Disks = append(out.Disks, AttachedDisk{DeviceName: d.DeviceName, Disk: last(d.Source), Boot: d.Boot, AutoDelete: d.AutoDelete})
	}
	return out
}

// Instances implements API.
func (c *Client) Instances(ctx context.Context, zone string) ([]Instance, error) {
	var out []Instance
	err := c.svc.Instances.List(c.project, zone).Context(ctx).Pages(ctx, func(l *gce.InstanceList) error {
		for _, i := range l.Items {
			out = append(out, instanceOf(i))
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// Instance implements API.
func (c *Client) Instance(ctx context.Context, zone, name string) (Instance, error) {
	i, err := c.svc.Instances.Get(c.project, zone, name).Context(ctx).Do()
	if err != nil {
		return Instance{}, classify(err)
	}
	return instanceOf(i), nil
}

func (c *Client) zonePath(zone string) string { return "projects/" + c.project + "/zones/" + zone }

// CreateInstance implements API. A Spot machine, and a standard one at its
// maximum run duration, stops rather than being deleted, so its disks are
// left for the service to deal with, unless DeleteAtMaxRun.
func (c *Client) CreateInstance(ctx context.Context, zone string, s InstanceSpec, requestID string) error {
	var items []*gce.MetadataItems
	for k, v := range s.Metadata {
		items = append(items, &gce.MetadataItems{Key: k, Value: new(v)})
	}
	sch := &gce.Scheduling{ProvisioningModel: "STANDARD", OnHostMaintenance: "MIGRATE", AutomaticRestart: new(false)}
	if s.Spot {
		sch = &gce.Scheduling{ProvisioningModel: "SPOT", OnHostMaintenance: "TERMINATE", AutomaticRestart: new(false),
			InstanceTerminationAction: "STOP"}
	}
	if s.MaxRunDuration > 0 {
		sch.MaxRunDuration = &gce.Duration{Seconds: int64(s.MaxRunDuration / time.Second)}
		sch.InstanceTerminationAction = "STOP"
		if s.DeleteAtMaxRun {
			sch.InstanceTerminationAction = "DELETE"
		}
	}
	imageProject := c.project
	if s.ImageProject != "" {
		imageProject = s.ImageProject
	}
	inst := &gce.Instance{
		Name:        s.Name,
		MachineType: c.zonePath(zone) + "/machineTypes/" + s.MachineType,
		Labels:      s.Labels,
		Metadata:    &gce.Metadata{Items: items},
		Disks: []*gce.AttachedDisk{{
			Boot: true, AutoDelete: true, DeviceName: "boot",
			InitializeParams: &gce.AttachedDiskInitializeParams{
				SourceImage: "projects/" + imageProject + "/global/images/" + s.Image,
				DiskSizeGb:  s.BootDiskGB, DiskType: c.zonePath(zone) + "/diskTypes/" + s.DiskType,
				ProvisionedIops: s.ProvisionedIOPS, ProvisionedThroughput: s.ProvisionedThroughput,
				Labels: s.Labels,
			},
		}},
		NetworkInterfaces: []*gce.NetworkInterface{{
			Network:       "projects/" + c.project + "/global/networks/" + s.Network,
			AccessConfigs: []*gce.AccessConfig{{Type: "ONE_TO_ONE_NAT", Name: "External NAT"}},
		}},
		Scheduling: sch,
	}
	if s.ServiceAccount != "" {
		inst.ServiceAccounts = []*gce.ServiceAccount{{Email: s.ServiceAccount, Scopes: s.Scopes}}
	}
	if s.Subnetwork != "" {
		inst.NetworkInterfaces[0].Subnetwork = "projects/" + c.project + "/regions/" + Region(zone) + "/subnetworks/" + s.Subnetwork
	}
	return c.change(ctx, zone, func() (*gce.Operation, error) {
		return c.svc.Instances.Insert(c.project, zone, inst).RequestId(requestID).Context(ctx).Do()
	})
}

// StopInstance implements API.
func (c *Client) StopInstance(ctx context.Context, zone, name, requestID string) error {
	return c.change(ctx, zone, func() (*gce.Operation, error) {
		return c.svc.Instances.Stop(c.project, zone, name).RequestId(requestID).Context(ctx).Do()
	})
}

// DeleteInstance implements API.
func (c *Client) DeleteInstance(ctx context.Context, zone, name, requestID string) error {
	return c.change(ctx, zone, func() (*gce.Operation, error) {
		return c.svc.Instances.Delete(c.project, zone, name).RequestId(requestID).Context(ctx).Do()
	})
}

// AttachDisk implements API. The disk goes with the instance when it's
// deleted.
func (c *Client) AttachDisk(ctx context.Context, zone, instance, disk, deviceName, requestID string) error {
	ad := &gce.AttachedDisk{Source: c.zonePath(zone) + "/disks/" + disk, DeviceName: deviceName, AutoDelete: true, Mode: "READ_WRITE"}
	return c.change(ctx, zone, func() (*gce.Operation, error) {
		return c.svc.Instances.AttachDisk(c.project, zone, instance, ad).RequestId(requestID).Context(ctx).Do()
	})
}

func diskOf(d *gce.Disk) Disk {
	out := Disk{Name: d.Name, ID: d.Id, Zone: last(d.Zone), SizeGB: d.SizeGb, Status: d.Status, Labels: d.Labels,
		Created: parseTime(d.CreationTimestamp)}
	for _, u := range d.Users {
		out.Users = append(out.Users, last(u))
	}
	return out
}

// Disks implements API.
func (c *Client) Disks(ctx context.Context, zone string) ([]Disk, error) {
	var out []Disk
	err := c.svc.Disks.List(c.project, zone).Context(ctx).Pages(ctx, func(l *gce.DiskList) error {
		for _, d := range l.Items {
			out = append(out, diskOf(d))
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// Disk implements API.
func (c *Client) Disk(ctx context.Context, zone, name string) (Disk, error) {
	d, err := c.svc.Disks.Get(c.project, zone, name).Context(ctx).Do()
	if err != nil {
		return Disk{}, classify(err)
	}
	return diskOf(d), nil
}

// CreateDisk implements API.
func (c *Client) CreateDisk(ctx context.Context, zone string, s DiskSpec, requestID string) error {
	d := &gce.Disk{Name: s.Name, SizeGb: s.SizeGB, Type: c.zonePath(zone) + "/diskTypes/" + s.DiskType,
		ProvisionedIops: s.ProvisionedIOPS, ProvisionedThroughput: s.ProvisionedThroughput, Labels: s.Labels}
	if s.SourceImage != "" {
		d.SourceImage = "projects/" + c.project + "/global/images/" + s.SourceImage
	}
	return c.change(ctx, zone, func() (*gce.Operation, error) {
		return c.svc.Disks.Insert(c.project, zone, d).RequestId(requestID).Context(ctx).Do()
	})
}

// DeleteDisk implements API.
func (c *Client) DeleteDisk(ctx context.Context, zone, name, requestID string) error {
	return c.change(ctx, zone, func() (*gce.Operation, error) {
		return c.svc.Disks.Delete(c.project, zone, name).RequestId(requestID).Context(ctx).Do()
	})
}

// Region is a zone's region.
func Region(zone string) string { return zone[:max(strings.LastIndex(zone, "-"), 0)] }

func imageOf(i *gce.Image) Image {
	return Image{Name: i.Name, ID: i.Id, Family: i.Family, Status: i.Status, Architecture: i.Architecture,
		DiskSizeGB: i.DiskSizeGb, SourceDisk: last(i.SourceDisk), Labels: i.Labels, LabelFingerprint: i.LabelFingerprint,
		Created: parseTime(i.CreationTimestamp)}
}

// Images implements API.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var out []Image
	err := c.svc.Images.List(c.project).Context(ctx).Pages(ctx, func(l *gce.ImageList) error {
		for _, i := range l.Items {
			out = append(out, imageOf(i))
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// Image implements API.
func (c *Client) Image(ctx context.Context, name string) (Image, error) {
	i, err := c.svc.Images.Get(c.project, name).Context(ctx).Do()
	if err != nil {
		return Image{}, classify(err)
	}
	return imageOf(i), nil
}

// CreateImage implements API. It waits for the image to be made, as long as
// ctx allows.
func (c *Client) CreateImage(ctx context.Context, s ImageSpec, requestID string) error {
	im := &gce.Image{Name: s.Name, Labels: s.Labels, SourceDisk: c.zonePath(s.SourceZone) + "/disks/" + s.SourceDisk}
	return c.change(ctx, "", func() (*gce.Operation, error) {
		return c.svc.Images.Insert(c.project, im).RequestId(requestID).Context(ctx).Do()
	})
}

// DeleteImage implements API.
func (c *Client) DeleteImage(ctx context.Context, name, requestID string) error {
	return c.change(ctx, "", func() (*gce.Operation, error) {
		return c.svc.Images.Delete(c.project, name).RequestId(requestID).Context(ctx).Do()
	})
}

// SetImageLabels implements API. A fingerprint that's no longer the
// image's is ErrFingerprint. Compute Engine takes no request ID for it; the
// fingerprint makes a repeated request harmless instead.
func (c *Client) SetImageLabels(ctx context.Context, name string, labels map[string]string, fingerprint string) error {
	req := &gce.GlobalSetLabelsRequest{Labels: labels, LabelFingerprint: fingerprint}
	return c.change(ctx, "", func() (*gce.Operation, error) {
		return c.svc.Images.SetLabels(c.project, name, req).Context(ctx).Do()
	})
}

// SerialOutput implements API.
func (c *Client) SerialOutput(ctx context.Context, zone, name string, limit int) (string, error) {
	var b strings.Builder
	var start int64
	for b.Len() < limit {
		out, err := c.svc.Instances.GetSerialPortOutput(c.project, zone, name).Port(1).Start(start).Context(ctx).Do()
		if err != nil {
			return "", classify(err)
		}
		b.WriteString(out.Contents)
		if out.Next <= start || out.Contents == "" {
			break
		}
		start = out.Next
	}
	s := b.String()
	if len(s) > limit {
		s = s[:limit]
	}
	return s, nil
}

// Preempted implements API.
func (c *Client) Preempted(ctx context.Context, zone string) ([]uint64, error) {
	var out []uint64
	err := c.svc.ZoneOperations.List(c.project, zone).Filter(`operationType = "compute.instances.preempted"`).Context(ctx).
		Pages(ctx, func(l *gce.OperationList) error {
			for _, op := range l.Items {
				if op.OperationType == "compute.instances.preempted" && op.TargetId != 0 {
					out = append(out, op.TargetId)
				}
			}
			return nil
		})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// ParseID reads a numeric instance ID as tokens and labels carry it.
func ParseID(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil && n > 0 && strconv.FormatUint(n, 10) == s
}
