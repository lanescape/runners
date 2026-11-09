package compute

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	gce "google.golang.org/api/compute/v1"

	"github.com/lanescape/runners/emulators/fault"
)

const timeFormat = "2006-01-02T15:04:05.000-07:00"

type operation struct {
	op     *gce.Operation
	zone   string // empty for a global operation
	doneAt time.Time
}

// apiError is an error as Compute Engine answers it.
type apiError struct {
	code    int
	reason  string
	message string
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.code, map[string]any{"error": map[string]any{
		"code": err.code, "message": err.message,
		"errors": []map[string]any{{"message": err.message, "domain": "global", "reason": err.reason}},
	}})
}

// JSONError answers as Compute Engine does with an error, for fault rules.
func JSONError(code int, reason, message string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { writeError(w, &apiError{code, reason, message}) }
}

func notFound(what string) *apiError {
	return &apiError{http.StatusNotFound, "notFound", fmt.Sprintf("The resource '%s' was not found", what)}
}

func invalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid", fmt.Sprintf(format, args...)}
}

func (e *Emulator) projectURL() string { return e.URL + "/compute/v1/projects/" + e.Project }
func (e *Emulator) zoneURL(z string) string {
	return e.projectURL() + "/zones/" + z
}
func (e *Emulator) instanceURL(in *instance) string {
	return e.zoneURL(in.zone) + "/instances/" + in.name
}
func (e *Emulator) diskURL(d *disk) string    { return e.zoneURL(d.zone) + "/disks/" + d.name }
func (e *Emulator) imageURL(im *image) string { return e.projectURL() + "/global/images/" + im.name }

func last(s string) string { return s[strings.LastIndex(s, "/")+1:] }

func (e *Emulator) routes() http.Handler {
	mux := http.NewServeMux()
	p := "/compute/v1/projects/{project}"
	z := p + "/zones/{zone}"
	mux.HandleFunc("GET "+z+"/instances", e.zoned(e.listInstances))
	mux.HandleFunc("POST "+z+"/instances", e.zoned(e.mutating(e.insertInstance)))
	mux.HandleFunc("GET "+z+"/instances/{name}", e.zoned(e.getInstance))
	mux.HandleFunc("DELETE "+z+"/instances/{name}", e.zoned(e.mutating(e.deleteInstance)))
	mux.HandleFunc("POST "+z+"/instances/{name}/stop", e.zoned(e.mutating(e.stopInstance)))
	mux.HandleFunc("POST "+z+"/instances/{name}/attachDisk", e.zoned(e.mutating(e.attachDisk)))
	mux.HandleFunc("POST "+z+"/instances/{name}/detachDisk", e.zoned(e.mutating(e.detachDisk)))
	mux.HandleFunc("POST "+z+"/instances/{name}/setMetadata", e.zoned(e.mutating(e.setMetadata)))
	mux.HandleFunc("POST "+z+"/instances/{name}/setLabels", e.zoned(e.mutating(e.setLabels)))
	mux.HandleFunc("GET "+z+"/instances/{name}/getGuestAttributes", e.zoned(e.getGuestAttributes))
	mux.HandleFunc("GET "+z+"/instances/{name}/serialPort", e.zoned(e.serialPort))
	mux.HandleFunc("GET "+z+"/disks", e.zoned(e.listDisks))
	mux.HandleFunc("POST "+z+"/disks", e.zoned(e.mutating(e.insertDisk)))
	mux.HandleFunc("GET "+z+"/disks/{name}", e.zoned(e.getDisk))
	mux.HandleFunc("DELETE "+z+"/disks/{name}", e.zoned(e.mutating(e.deleteDisk)))
	mux.HandleFunc("GET "+z+"/operations", e.zoned(e.listOperations))
	mux.HandleFunc("GET "+z+"/operations/{op}", e.zoned(e.getOperation))
	mux.HandleFunc("POST "+z+"/operations/{op}/wait", e.zoned(e.waitOperation))
	mux.HandleFunc("GET "+p+"/global/images", e.global(e.listImages))
	mux.HandleFunc("POST "+p+"/global/images", e.global(e.mutating(e.insertImage)))
	mux.HandleFunc("GET "+p+"/global/images/{name}", e.global(e.getImage))
	mux.HandleFunc("DELETE "+p+"/global/images/{name}", e.global(e.mutating(e.deleteImage)))
	mux.HandleFunc("POST "+p+"/global/images/{name}/setLabels", e.global(e.mutating(e.setImageLabels)))
	mux.HandleFunc("GET "+p+"/global/images/family/{family}", e.global(e.getImageFromFamily))
	mux.HandleFunc("GET "+p+"/global/operations/{op}", e.global(e.getOperation))
	mux.HandleFunc("POST "+p+"/global/operations/{op}/wait", e.global(e.waitOperation))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{http.StatusNotFound, "notFound", "The requested URL " + r.URL.Path + " was not found."})
	})
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, zone string)

// zoned checks the project and zone a request names, and holds e.mu while h
// runs.
func (e *Emulator) zoned(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pr := r.PathValue("project"); pr != e.Project && pr != strconv.FormatInt(e.ProjectNumber, 10) {
			writeError(w, &apiError{http.StatusForbidden, "forbidden", fmt.Sprintf("Required permission for 'projects/%s'", pr)})
			return
		}
		zone := r.PathValue("zone")
		if !slices.Contains(e.zones, zone) {
			writeError(w, notFound("projects/"+e.Project+"/zones/"+zone))
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.tick()
		h(w, r, zone)
	}
}

// global is zoned for the project's global resources.
func (e *Emulator) global(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pr := r.PathValue("project"); pr != e.Project && pr != strconv.FormatInt(e.ProjectNumber, 10) {
			writeError(w, &apiError{http.StatusForbidden, "forbidden", fmt.Sprintf("Required permission for 'projects/%s'", pr)})
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.tick()
		h(w, r, "")
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// mutating gives a change its requestId's handling: a request with a
// requestId already seen gets that request's operation, and nothing is done
// again.
func (e *Emulator) mutating(do func(r *http.Request, zone string) (*operation, *apiError)) handler {
	return func(w http.ResponseWriter, r *http.Request, zone string) {
		rid := r.URL.Query().Get("requestId")
		if rid != "" {
			if !uuidPattern.MatchString(rid) || rid == "00000000-0000-0000-0000-000000000000" {
				writeError(w, invalid("Invalid value for field 'requestId': '%s'. The requestId must be a valid UUID.", rid))
				return
			}
			if op := e.requests[rid]; op != nil {
				writeJSON(w, http.StatusOK, e.opJSON(op))
				return
			}
		}
		op, err := do(r, zone)
		if err != nil {
			writeError(w, err)
			return
		}
		if rid != "" {
			e.requests[rid] = op
		}
		writeJSON(w, http.StatusOK, e.opJSON(op))
	}
}

func (e *Emulator) newOp(zone, typ, target string, targetID uint64) *operation {
	now := e.clock.Now()
	var b [6]byte
	_, _ = rand.Read(b[:])
	op := &gce.Operation{
		Kind: "compute#operation", Id: newID(), Name: fmt.Sprintf("operation-%d-%x", now.UnixNano(), b),
		OperationType: typ, TargetLink: target, TargetId: targetID, InsertTime: now.Format(timeFormat),
		StartTime: now.Format(timeFormat), User: "lanescape-runners@" + e.Project + ".iam.gserviceaccount.com",
	}
	o := &operation{op: op, zone: zone, doneAt: time.Now().Add(e.opDelay)}
	if zone != "" {
		op.Zone = e.zoneURL(zone)
		op.SelfLink = e.zoneURL(zone) + "/operations/" + op.Name
	} else {
		op.SelfLink = e.projectURL() + "/global/operations/" + op.Name
	}
	e.ops[op.Name] = o
	return o
}

// systemOp records an operation Compute Engine did itself, such as a
// preemption, done at once. The caller holds e.mu.
func (e *Emulator) systemOp(in *instance, typ string) {
	o := e.newOp(in.zone, typ, e.instanceURL(in), in.id)
	o.op.User = "system"
	o.doneAt = time.Time{}
}

// fail makes an operation end with an error, as Compute Engine reports what
// it couldn't do after accepting a request.
func (o *operation) fail(httpCode int, code, message string) *operation {
	o.op.Error = &gce.OperationError{Errors: []*gce.OperationErrorErrors{{Code: code, Message: message}}}
	o.op.HttpErrorStatusCode = int64(httpCode)
	o.op.HttpErrorMessage = strings.ToUpper(http.StatusText(httpCode))
	return o
}

func (e *Emulator) opJSON(o *operation) *gce.Operation {
	c := *o.op
	if time.Now().Before(o.doneAt) {
		c.Status, c.Progress, c.Error, c.HttpErrorStatusCode, c.HttpErrorMessage = "RUNNING", 50, nil, 0, ""
		return &c
	}
	c.Status, c.Progress = "DONE", 100
	c.EndTime = c.StartTime
	return &c
}

func (e *Emulator) findOp(w http.ResponseWriter, r *http.Request, zone string) *operation {
	o := e.ops[r.PathValue("op")]
	if o == nil || o.zone != zone {
		where := "projects/" + e.Project + "/global"
		if zone != "" {
			where = "projects/" + e.Project + "/zones/" + zone
		}
		writeError(w, notFound(where+"/operations/"+r.PathValue("op")))
		return nil
	}
	return o
}

func (e *Emulator) getOperation(w http.ResponseWriter, r *http.Request, zone string) {
	if o := e.findOp(w, r, zone); o != nil {
		writeJSON(w, http.StatusOK, e.opJSON(o))
	}
}

// waitOperation answers once the operation is done, or after a while as
// Compute Engine does, whichever is first.
func (e *Emulator) waitOperation(w http.ResponseWriter, r *http.Request, zone string) {
	o := e.findOp(w, r, zone)
	if o == nil {
		return
	}
	if d := time.Until(o.doneAt); d > 0 {
		e.mu.Unlock()
		t := time.NewTimer(min(d, 2*time.Minute))
		select {
		case <-t.C:
		case <-r.Context().Done():
		}
		t.Stop()
		e.mu.Lock()
	}
	writeJSON(w, http.StatusOK, e.opJSON(o))
}

var opFilter = regexp.MustCompile(`^\s*\(?\s*operationType\s*(?:=|eq)\s*"?([A-Za-z0-9._]+)"?\s*\)?\s*$`)

func (e *Emulator) listOperations(w http.ResponseWriter, r *http.Request, zone string) {
	typ := ""
	if f := r.URL.Query().Get("filter"); f != "" {
		m := opFilter.FindStringSubmatch(f)
		if m == nil {
			writeError(w, invalid("Invalid value for field 'filter': '%s'.", f))
			return
		}
		typ = m[1]
	}
	var ops []*operation
	for _, o := range e.ops {
		if o.zone == zone && (typ == "" || o.op.OperationType == typ) {
			ops = append(ops, o)
		}
	}
	slices.SortFunc(ops, func(a, b *operation) int { return strings.Compare(a.op.Name, b.op.Name) })
	items, next := paged(r, ops)
	list := &gce.OperationList{Kind: "compute#operationList", NextPageToken: next, SelfLink: e.zoneURL(zone) + "/operations"}
	for _, o := range items {
		list.Items = append(list.Items, e.opJSON(o))
	}
	writeJSON(w, http.StatusOK, list)
}

// ForgetOperations drops every finished operation, as Compute Engine does
// after as little as an hour.
func (e *Emulator) ForgetOperations() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, o := range e.ops {
		if !time.Now().Before(o.doneAt) {
			delete(e.ops, name)
		}
	}
}

// paged cuts a list by maxResults and pageToken, as Compute Engine does.
func paged[T any](r *http.Request, items []T) ([]T, string) {
	per := 500
	if n, err := strconv.Atoi(r.URL.Query().Get("maxResults")); err == nil && n > 0 && n < per {
		per = n
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	if start > len(items) {
		start = len(items)
	}
	end := min(start+per, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next
}

var (
	resourceName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	labelKey     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	labelValue   = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
	metadataKey  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

func checkLabels(labels map[string]string) *apiError {
	if len(labels) > 64 {
		return invalid("Invalid value for field 'labels': too many labels.")
	}
	for k, v := range labels {
		if !labelKey.MatchString(k) || !labelValue.MatchString(v) {
			return invalid("Invalid value for field 'labels': '%s: %s'. Label keys and values must be lowercase letters, digits, underscores and hyphens.", k, v)
		}
	}
	return nil
}

func checkMetadata(md *gce.Metadata) ([]metadataItem, *apiError) {
	var items []metadataItem
	total := 0
	if md == nil {
		return nil, nil
	}
	for _, it := range md.Items {
		if !metadataKey.MatchString(it.Key) {
			return nil, invalid("Invalid value for field 'metadata.items.key': '%s'.", it.Key)
		}
		v := ""
		if it.Value != nil {
			v = *it.Value
		}
		if len(v) > 256<<10 {
			return nil, invalid("Metadata value for %s is too large.", it.Key)
		}
		total += len(it.Key) + len(v)
		items = append(items, metadataItem{it.Key, v})
	}
	if total > 512<<10 {
		return nil, invalid("Metadata is too large.")
	}
	return items, nil
}

func fingerprint(n int64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	return base64.StdEncoding.EncodeToString(b[:])
}

func (e *Emulator) instanceJSON(in *instance) *gce.Instance {
	e.running(in)
	out := &gce.Instance{
		Kind: "compute#instance", Id: in.id, Name: in.name, Zone: e.zoneURL(in.zone),
		MachineType: e.zoneURL(in.zone) + "/machineTypes/" + in.machineType, Status: in.status,
		CreationTimestamp: in.created.Format(timeFormat), Labels: in.labels, LabelFingerprint: fingerprint(in.labelFP),
		Metadata: &gce.Metadata{Kind: "compute#metadata", Fingerprint: fingerprint(in.metaFP)},
		SelfLink: e.instanceURL(in),
		Scheduling: &gce.Scheduling{ProvisioningModel: in.provisioning, InstanceTerminationAction: in.termination,
			OnHostMaintenance: in.hostMaint, AutomaticRestart: new(in.provisioning != "SPOT")},
	}
	if in.maxRun > 0 {
		out.Scheduling.MaxRunDuration = &gce.Duration{Seconds: int64(in.maxRun / time.Second)}
	}
	for _, it := range in.metadata {
		out.Metadata.Items = append(out.Metadata.Items, &gce.MetadataItems{Key: it.key, Value: new(it.value)})
	}
	for _, a := range in.disks {
		out.Disks = append(out.Disks, &gce.AttachedDisk{Kind: "compute#attachedDisk", Boot: a.boot, AutoDelete: a.autoDelete,
			DeviceName: a.deviceName, Index: a.index, Source: e.diskURL(a.disk), Mode: "READ_WRITE", Type: "PERSISTENT",
			Interface: "NVME", DiskSizeGb: a.disk.sizeGB, Architecture: a.disk.arch})
	}
	ni := &gce.NetworkInterface{Kind: "compute#networkInterface", Name: "nic0", Network: e.projectURL() + "/global/networks/" + in.network}
	if in.externalIP {
		ni.AccessConfigs = []*gce.AccessConfig{{Kind: "compute#accessConfig", Type: "ONE_TO_ONE_NAT", Name: "External NAT"}}
	}
	out.NetworkInterfaces = []*gce.NetworkInterface{ni}
	if in.serviceAccount != "" {
		out.ServiceAccounts = []*gce.ServiceAccount{{Email: in.serviceAccount, Scopes: in.scopes}}
	}
	return out
}

func (e *Emulator) findInstance(w http.ResponseWriter, r *http.Request, zone string) *instance {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		writeError(w, notFound("projects/"+e.Project+"/zones/"+zone+"/instances/"+r.PathValue("name")))
	}
	return in
}

func (e *Emulator) listInstances(w http.ResponseWriter, r *http.Request, zone string) {
	var ins []*instance
	for _, in := range e.instances {
		if in.zone == zone {
			ins = append(ins, in)
		}
	}
	slices.SortFunc(ins, func(a, b *instance) int { return strings.Compare(a.name, b.name) })
	items, next := paged(r, ins)
	list := &gce.InstanceList{Kind: "compute#instanceList", NextPageToken: next, SelfLink: e.zoneURL(zone) + "/instances"}
	for _, in := range items {
		list.Items = append(list.Items, e.instanceJSON(in))
	}
	writeJSON(w, http.StatusOK, list)
}

func (e *Emulator) getInstance(w http.ResponseWriter, r *http.Request, zone string) {
	if in := e.findInstance(w, r, zone); in != nil {
		writeJSON(w, http.StatusOK, e.instanceJSON(in))
	}
}

func decode(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil || json.Unmarshal(body, v) != nil {
		return &apiError{http.StatusBadRequest, "parseError", "This API does not support parsing form-encoded input."}
	}
	return nil
}

// resolveImage finds an image by any of the ways a request may name it.
func (e *Emulator) resolveImage(name string) (*image, *apiError) {
	i := strings.Index(name, "global/images/")
	if i < 0 {
		return nil, invalid("Invalid value for field 'sourceImage': '%s'.", name)
	}
	rest := name[i+len("global/images/"):]
	if fam, ok := strings.CutPrefix(rest, "family/"); ok {
		var best *image
		for _, im := range e.images {
			if im.family == fam && im.status == "READY" && (best == nil || im.created.After(best.created)) {
				best = im
			}
		}
		if best == nil {
			return nil, notFound("projects/" + e.Project + "/global/images/family/" + fam)
		}
		return best, nil
	}
	im := e.images[rest]
	if im == nil {
		return nil, notFound("projects/" + e.Project + "/global/images/" + rest)
	}
	if im.status != "READY" {
		return nil, &apiError{http.StatusBadRequest, "resourceNotReady", fmt.Sprintf("The resource '%s' is not ready", e.imageURL(im))}
	}
	return im, nil
}

// checkDiskType checks a disk's type, which a Hyperdisk-only family's
// machines insist on.
func checkDiskType(typ, family string) *apiError {
	switch typ {
	case "hyperdisk-balanced", "hyperdisk-throughput", "hyperdisk-extreme", "pd-balanced", "pd-ssd", "pd-standard":
	default:
		return invalid("Invalid value for field 'diskType': '%s'.", typ)
	}
	if family != "" && hyperdiskOnly[family] && !strings.HasPrefix(typ, "hyperdisk-") {
		return invalid("[%s] disk type cannot be used by %s machine type.", typ, family)
	}
	return nil
}

func (e *Emulator) insertInstance(r *http.Request, zone string) (*operation, *apiError) {
	var body gce.Instance
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if !resourceName.MatchString(body.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'.", body.Name)
	}
	where := "projects/" + e.Project + "/zones/" + zone + "/instances/" + body.Name
	if e.instances[zone+"/"+body.Name] != nil {
		return nil, &apiError{http.StatusConflict, "alreadyExists", fmt.Sprintf("The resource '%s' already exists", where)}
	}
	mt := last(body.MachineType)
	family, vcpus, ok := machineShape(mt)
	if !ok {
		return nil, invalid("Invalid value for field 'resource.machineType': '%s'. Machine type with name '%s' does not exist in zone '%s'.", body.MachineType, mt, zone)
	}
	if err := checkLabels(body.Labels); err != nil {
		return nil, err
	}
	items, err := checkMetadata(body.Metadata)
	if err != nil {
		return nil, err
	}
	sch := body.Scheduling
	if sch == nil {
		sch = &gce.Scheduling{}
	}
	prov := sch.ProvisioningModel
	if prov == "" {
		prov = "STANDARD"
	}
	if prov != "STANDARD" && prov != "SPOT" {
		return nil, invalid("Invalid value for field 'resource.scheduling.provisioningModel': '%s'.", prov)
	}
	var maxRun time.Duration
	if sch.MaxRunDuration != nil {
		maxRun = time.Duration(sch.MaxRunDuration.Seconds) * time.Second
	}
	if (prov == "SPOT" || maxRun > 0) && sch.InstanceTerminationAction != "STOP" && sch.InstanceTerminationAction != "DELETE" {
		return nil, invalid("Invalid value for field 'resource.scheduling.instanceTerminationAction': '%s'. A Spot machine, or one with a maximum run duration, needs STOP or DELETE.", sch.InstanceTerminationAction)
	}
	if prov == "SPOT" && ((sch.OnHostMaintenance != "" && sch.OnHostMaintenance != "TERMINATE") || (sch.AutomaticRestart != nil && *sch.AutomaticRestart)) {
		return nil, invalid("Scheduling must have onHostMaintenance TERMINATE and automaticRestart false for a Spot machine.")
	}
	var boots []*gce.AttachedDisk
	for _, d := range body.Disks {
		if d.Boot {
			boots = append(boots, d)
		}
	}
	if len(boots) != 1 || boots[0].InitializeParams == nil {
		return nil, invalid("Invalid value for field 'resource.disks': exactly one boot disk with initializeParams is required.")
	}
	if len(body.Disks) != 1 {
		return nil, invalid("Invalid value for field 'resource.disks': only a new boot disk is emulated.")
	}
	ip := boots[0].InitializeParams
	im, err := e.resolveImage(ip.SourceImage)
	if err != nil {
		return nil, err
	}
	if im.arch != archOf(family) {
		return nil, invalid("The image %s's architecture %s doesn't match machine type %s.", im.name, im.arch, mt)
	}
	typ := "pd-balanced"
	if ip.DiskType != "" {
		typ = last(ip.DiskType)
	}
	if err := checkDiskType(typ, family); err != nil {
		return nil, err
	}
	if ip.DiskSizeGb != 0 && ip.DiskSizeGb < im.sizeGB {
		return nil, invalid("Requested disk size cannot be smaller than the image size (%d GB)", im.sizeGB)
	}
	bootName := ip.DiskName
	if bootName == "" {
		bootName = body.Name
	}
	if e.disks[zone+"/"+bootName] != nil {
		return nil, &apiError{http.StatusConflict, "alreadyExists", fmt.Sprintf("The resource 'projects/%s/zones/%s/disks/%s' already exists", e.Project, zone, bootName)}
	}
	sa, scopes := "", []string(nil)
	if len(body.ServiceAccounts) > 0 {
		sa, scopes = body.ServiceAccounts[0].Email, body.ServiceAccounts[0].Scopes
		if !strings.HasSuffix(sa, "@"+e.Project+".iam.gserviceaccount.com") {
			return nil, invalid("The service account '%s' isn't one of the project's.", sa)
		}
	}
	network, subnetwork, external := "default", "", false
	if len(body.NetworkInterfaces) > 0 {
		ni := body.NetworkInterfaces[0]
		network, external = last(ni.Network), len(ni.AccessConfigs) > 0
		if ni.Subnetwork != "" {
			if !strings.Contains(ni.Subnetwork, "/regions/"+region(zone)+"/subnetworks/") {
				return nil, invalid("Invalid value for field 'resource.networkInterfaces[0].subnetwork': '%s'. The subnetwork must be in the instance's region.", ni.Subnetwork)
			}
			subnetwork = last(ni.Subnetwork)
		}
	}

	// Quota is checked as the request arrives; capacity once it's accepted.
	metric := strings.ToUpper(family) + "_CPUS"
	if _, ok := e.quotas[region(zone)+"/PREEMPTIBLE_CPUS"]; ok && prov == "SPOT" {
		metric = "PREEMPTIBLE_CPUS"
	}
	if limit, ok := e.quotas[region(zone)+"/"+metric]; ok {
		used := vcpus
		for _, other := range e.instances {
			f, n, _ := machineShape(other.machineType)
			om := strings.ToUpper(f) + "_CPUS"
			if metric == "PREEMPTIBLE_CPUS" {
				om = map[bool]string{true: "PREEMPTIBLE_CPUS", false: ""}[other.provisioning == "SPOT"]
			}
			if region(other.zone) == region(zone) && om == metric && other.status != "TERMINATED" {
				used += n
			}
		}
		if used > limit {
			return nil, &apiError{http.StatusForbidden, "quotaExceeded",
				fmt.Sprintf("Quota '%s' exceeded.  Limit: %d.0 in region %s.", metric, limit, region(zone))}
		}
	}
	if e.refusing[capacityKey{zone, mt, prov == "SPOT"}] {
		return nil, &apiError{http.StatusServiceUnavailable, "insufficientCapacity",
			fmt.Sprintf("The zone 'projects/%s/zones/%s' does not have enough resources available to fulfill the request.", e.Project, zone)}
	}
	op := e.newOp(zone, "insert", e.zoneURL(zone)+"/instances/"+body.Name, 0)
	if e.stockouts[capacityKey{zone, mt, prov == "SPOT"}] {
		return op.fail(http.StatusServiceUnavailable, "ZONE_RESOURCE_POOL_EXHAUSTED",
			fmt.Sprintf("The zone 'projects/%s/zones/%s' does not have enough resources available to fulfill the request.  Try a different zone, or try again later.", e.Project, zone)), nil
	}
	now := e.clock.Now()
	in := &instance{id: newID(), name: body.Name, zone: zone, machineType: mt, status: "PROVISIONING", created: now,
		readyAt: time.Now().Add(e.opDelay), labels: body.Labels, metadata: items, network: network, subnetwork: subnetwork, externalIP: external,
		serviceAccount: sa, scopes: scopes, provisioning: prov, termination: sch.InstanceTerminationAction, maxRun: maxRun,
		hostMaint: sch.OnHostMaintenance, guest: map[string]string{}}
	op.op.TargetId = in.id
	size := ip.DiskSizeGb
	if size == 0 {
		size = im.sizeGB
	}
	d := &disk{id: newID(), name: bootName, zone: zone, typ: typ, sizeGB: size, iops: ip.ProvisionedIops,
		throughput: ip.ProvisionedThroughput, labels: ip.Labels, sourceImage: e.imageURL(im), arch: im.arch, created: now,
		users: []*instance{in}}
	e.disks[zone+"/"+bootName] = d
	dev := boots[0].DeviceName
	if dev == "" {
		dev = "persistent-disk-0"
	}
	in.disks = []*attached{{deviceName: dev, disk: d, boot: true, autoDelete: boots[0].AutoDelete}}
	e.instances[zone+"/"+in.name] = in
	e.running(in)
	return op, nil
}

func (e *Emulator) deleteInstance(r *http.Request, zone string) (*operation, *apiError) {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	op := e.newOp(zone, "delete", e.instanceURL(in), in.id)
	if code, ok := e.failing("instances.delete"); ok {
		return op.fail(http.StatusInternalServerError, code, "The operation failed."), nil
	}
	e.remove(in)
	return op, nil
}

func (e *Emulator) stopInstance(r *http.Request, zone string) (*operation, *apiError) {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	op := e.newOp(zone, "stop", e.instanceURL(in), in.id)
	if in.status != "TERMINATED" {
		e.halt(in, "TERMINATED")
	}
	return op, nil
}

func (e *Emulator) attachDisk(r *http.Request, zone string) (*operation, *apiError) {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	var body gce.AttachedDisk
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	i := strings.Index(body.Source, "zones/")
	if i < 0 {
		return nil, invalid("Invalid value for field 'resource.source': '%s'.", body.Source)
	}
	parts := strings.Split(body.Source[i:], "/")
	if len(parts) != 4 || parts[2] != "disks" {
		return nil, invalid("Invalid value for field 'resource.source': '%s'.", body.Source)
	}
	if parts[1] != zone {
		return nil, invalid("Disk must be in the same zone as the instance.")
	}
	d := e.disks[zone+"/"+parts[3]]
	if d == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/disks/" + parts[3])
	}
	if len(d.users) > 0 {
		return nil, &apiError{http.StatusBadRequest, "resourceInUseByAnotherResource",
			fmt.Sprintf("The disk resource '%s' is already being used by '%s'", e.diskURL(d), e.instanceURL(d.users[0]))}
	}
	dev := body.DeviceName
	if dev == "" {
		dev = d.name
	}
	for _, a := range in.disks {
		if a.deviceName == dev {
			return nil, invalid("Invalid value for field 'resource.deviceName': '%s'. The device name is already in use.", dev)
		}
	}
	op := e.newOp(zone, "attachDisk", e.instanceURL(in), in.id)
	d.users = append(d.users, in)
	in.disks = append(in.disks, &attached{deviceName: dev, disk: d, autoDelete: body.AutoDelete, index: int64(len(in.disks))})
	return op, nil
}

func (e *Emulator) detachDisk(r *http.Request, zone string) (*operation, *apiError) {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	dev := r.URL.Query().Get("deviceName")
	i := slices.IndexFunc(in.disks, func(a *attached) bool { return a.deviceName == dev })
	if i < 0 {
		return nil, invalid("Invalid value for field 'deviceName': '%s'. No attached disk found with that device name.", dev)
	}
	if in.disks[i].boot {
		return nil, invalid("The boot disk can't be detached.")
	}
	op := e.newOp(zone, "detachDisk", e.instanceURL(in), in.id)
	d := in.disks[i].disk
	d.users = slices.DeleteFunc(d.users, func(u *instance) bool { return u == in })
	in.disks = slices.Delete(in.disks, i, i+1)
	return op, nil
}

func (e *Emulator) setMetadata(r *http.Request, zone string) (*operation, *apiError) {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	var body gce.Metadata
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Fingerprint != fingerprint(in.metaFP) {
		return nil, &apiError{http.StatusPreconditionFailed, "conditionNotMet", "Supplied fingerprint does not match current metadata fingerprint."}
	}
	items, err := checkMetadata(&body)
	if err != nil {
		return nil, err
	}
	op := e.newOp(zone, "setMetadata", e.instanceURL(in), in.id)
	in.metadata, in.metaFP = items, in.metaFP+1
	return op, nil
}

func (e *Emulator) setLabels(r *http.Request, zone string) (*operation, *apiError) {
	in := e.instances[zone+"/"+r.PathValue("name")]
	if in == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	var body gce.InstancesSetLabelsRequest
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.LabelFingerprint != fingerprint(in.labelFP) {
		return nil, &apiError{http.StatusPreconditionFailed, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed"}
	}
	if err := checkLabels(body.Labels); err != nil {
		return nil, err
	}
	op := e.newOp(zone, "setLabels", e.instanceURL(in), in.id)
	in.labels, in.labelFP = body.Labels, in.labelFP+1
	return op, nil
}

func (e *Emulator) getGuestAttributes(w http.ResponseWriter, r *http.Request, zone string) {
	in := e.findInstance(w, r, zone)
	if in == nil {
		return
	}
	q := r.URL.Query()
	out := &gce.GuestAttributes{Kind: "compute#guestAttributes", SelfLink: e.instanceURL(in) + "/getGuestAttributes"}
	if key := q.Get("variableKey"); key != "" {
		v, ok := in.guest[key]
		if !ok {
			writeError(w, notFound("projects/"+e.Project+"/zones/"+zone+"/instances/"+in.name+"/guestAttributes/"+key))
			return
		}
		out.VariableKey, out.VariableValue = key, v
		writeJSON(w, http.StatusOK, out)
		return
	}
	path := q.Get("queryPath")
	out.QueryPath = path
	out.QueryValue = &gce.GuestAttributesValue{}
	var keys []string
	for k := range in.guest {
		if strings.HasPrefix(k, path) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		writeError(w, notFound("projects/"+e.Project+"/zones/"+zone+"/instances/"+in.name+"/guestAttributes/"+path))
		return
	}
	for _, k := range keys {
		ns, key, _ := strings.Cut(k, "/")
		out.QueryValue.Items = append(out.QueryValue.Items, &gce.GuestAttributesEntry{Namespace: ns, Key: key, Value: in.guest[k]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Emulator) serialPort(w http.ResponseWriter, r *http.Request, zone string) {
	in := e.findInstance(w, r, zone)
	if in == nil {
		return
	}
	if p := r.URL.Query().Get("port"); p != "" && p != "1" {
		writeJSON(w, http.StatusOK, &gce.SerialPortOutput{Kind: "compute#serialPortOutput"})
		return
	}
	s := in.serial.String()
	start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
	start = max(0, min(start, int64(len(s))))
	// Compute Engine answers at most 1 MiB at a time.
	end := min(int64(len(s)), start+1<<20)
	writeJSON(w, http.StatusOK, &gce.SerialPortOutput{Kind: "compute#serialPortOutput", Contents: s[start:end], Start: start, Next: end,
		SelfLink: e.instanceURL(in) + "/serialPort"})
}

func (e *Emulator) diskJSON(d *disk) *gce.Disk {
	out := &gce.Disk{Kind: "compute#disk", Id: d.id, Name: d.name, Zone: e.zoneURL(d.zone), SizeGb: d.sizeGB,
		Type: e.zoneURL(d.zone) + "/diskTypes/" + d.typ, Status: "READY", Labels: d.labels, SourceImage: d.sourceImage,
		ProvisionedIops: d.iops, ProvisionedThroughput: d.throughput, CreationTimestamp: d.created.Format(timeFormat),
		SelfLink: e.diskURL(d), Architecture: d.arch}
	for _, u := range d.users {
		out.Users = append(out.Users, e.instanceURL(u))
	}
	return out
}

func (e *Emulator) listDisks(w http.ResponseWriter, r *http.Request, zone string) {
	var ds []*disk
	for _, d := range e.disks {
		if d.zone == zone {
			ds = append(ds, d)
		}
	}
	slices.SortFunc(ds, func(a, b *disk) int { return strings.Compare(a.name, b.name) })
	items, next := paged(r, ds)
	list := &gce.DiskList{Kind: "compute#diskList", NextPageToken: next, SelfLink: e.zoneURL(zone) + "/disks"}
	for _, d := range items {
		list.Items = append(list.Items, e.diskJSON(d))
	}
	writeJSON(w, http.StatusOK, list)
}

func (e *Emulator) getDisk(w http.ResponseWriter, r *http.Request, zone string) {
	d := e.disks[zone+"/"+r.PathValue("name")]
	if d == nil {
		writeError(w, notFound("projects/"+e.Project+"/zones/"+zone+"/disks/"+r.PathValue("name")))
		return
	}
	writeJSON(w, http.StatusOK, e.diskJSON(d))
}

func (e *Emulator) insertDisk(r *http.Request, zone string) (*operation, *apiError) {
	var body gce.Disk
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if !resourceName.MatchString(body.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'.", body.Name)
	}
	if e.disks[zone+"/"+body.Name] != nil {
		return nil, &apiError{http.StatusConflict, "alreadyExists", fmt.Sprintf("The resource 'projects/%s/zones/%s/disks/%s' already exists", e.Project, zone, body.Name)}
	}
	if err := checkLabels(body.Labels); err != nil {
		return nil, err
	}
	typ := "pd-standard"
	if body.Type != "" {
		typ = last(body.Type)
	}
	if err := checkDiskType(typ, ""); err != nil {
		return nil, err
	}
	d := &disk{id: newID(), name: body.Name, zone: zone, typ: typ, sizeGB: body.SizeGb, iops: body.ProvisionedIops,
		throughput: body.ProvisionedThroughput, labels: body.Labels, created: e.clock.Now(), arch: body.Architecture}
	if body.SourceImage != "" {
		im, err := e.resolveImage(body.SourceImage)
		if err != nil {
			return nil, err
		}
		d.sourceImage, d.arch = e.imageURL(im), im.arch
		if d.sizeGB == 0 {
			d.sizeGB = im.sizeGB
		}
		if d.sizeGB < im.sizeGB {
			return nil, invalid("Requested disk size cannot be smaller than the image size (%d GB)", im.sizeGB)
		}
	}
	if d.sizeGB <= 0 {
		return nil, invalid("Invalid value for field 'resource.sizeGb': '%d'.", d.sizeGB)
	}
	op := e.newOp(zone, "insert", e.diskURL(d), d.id)
	if code, ok := e.failing("disks.insert"); ok {
		return op.fail(http.StatusInternalServerError, code, "The operation failed."), nil
	}
	if e.diskShort[zone] {
		return op.fail(http.StatusServiceUnavailable, "ZONE_RESOURCE_POOL_EXHAUSTED",
			fmt.Sprintf("The zone 'projects/%s/zones/%s' does not have enough resources available to fulfill the request.", e.Project, zone)), nil
	}
	e.disks[zone+"/"+d.name] = d
	return op, nil
}

func (e *Emulator) deleteDisk(r *http.Request, zone string) (*operation, *apiError) {
	d := e.disks[zone+"/"+r.PathValue("name")]
	if d == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + zone + "/disks/" + r.PathValue("name"))
	}
	if len(d.users) > 0 {
		return nil, &apiError{http.StatusBadRequest, "resourceInUseByAnotherResource",
			fmt.Sprintf("The disk resource '%s' is already being used by '%s'", e.diskURL(d), e.instanceURL(d.users[0]))}
	}
	op := e.newOp(zone, "delete", e.diskURL(d), d.id)
	delete(e.disks, zone+"/"+d.name)
	return op, nil
}

func (e *Emulator) imageJSON(im *image) *gce.Image {
	return &gce.Image{Kind: "compute#image", Id: im.id, Name: im.name, Family: im.family, Status: im.status,
		Architecture: im.arch, Labels: im.labels, LabelFingerprint: fingerprint(im.labelFP), CreationTimestamp: im.created.Format(timeFormat),
		SourceDisk: im.sourceDisk, DiskSizeGb: im.sizeGB, SelfLink: e.imageURL(im)}
}

func (e *Emulator) listImages(w http.ResponseWriter, r *http.Request, _ string) {
	var ims []*image
	for _, im := range e.images {
		ims = append(ims, im)
	}
	slices.SortFunc(ims, func(a, b *image) int { return strings.Compare(a.name, b.name) })
	items, next := paged(r, ims)
	list := &gce.ImageList{Kind: "compute#imageList", NextPageToken: next, SelfLink: e.projectURL() + "/global/images"}
	for _, im := range items {
		list.Items = append(list.Items, e.imageJSON(im))
	}
	writeJSON(w, http.StatusOK, list)
}

func (e *Emulator) getImage(w http.ResponseWriter, r *http.Request, _ string) {
	im := e.images[r.PathValue("name")]
	if im == nil {
		writeError(w, notFound("projects/"+e.Project+"/global/images/"+r.PathValue("name")))
		return
	}
	writeJSON(w, http.StatusOK, e.imageJSON(im))
}

func (e *Emulator) getImageFromFamily(w http.ResponseWriter, r *http.Request, _ string) {
	im, err := e.resolveImage("global/images/family/" + r.PathValue("family"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e.imageJSON(im))
}

func (e *Emulator) insertImage(r *http.Request, _ string) (*operation, *apiError) {
	var body gce.Image
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if !resourceName.MatchString(body.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'.", body.Name)
	}
	if e.images[body.Name] != nil {
		return nil, &apiError{http.StatusConflict, "alreadyExists", fmt.Sprintf("The resource 'projects/%s/global/images/%s' already exists", e.Project, body.Name)}
	}
	if err := checkLabels(body.Labels); err != nil {
		return nil, err
	}
	i := strings.Index(body.SourceDisk, "zones/")
	if i < 0 {
		return nil, invalid("Invalid value for field 'resource.sourceDisk': '%s'.", body.SourceDisk)
	}
	parts := strings.Split(body.SourceDisk[i:], "/")
	if len(parts) != 4 {
		return nil, invalid("Invalid value for field 'resource.sourceDisk': '%s'.", body.SourceDisk)
	}
	d := e.disks[parts[1]+"/"+parts[3]]
	if d == nil {
		return nil, notFound("projects/" + e.Project + "/zones/" + parts[1] + "/disks/" + parts[3])
	}
	for _, u := range d.users {
		if u.status == "RUNNING" && r.URL.Query().Get("forceCreate") != "true" {
			return nil, &apiError{http.StatusBadRequest, "resourceInUseByAnotherResource",
				fmt.Sprintf("The disk resource '%s' is already being used by '%s'", e.diskURL(d), e.instanceURL(u))}
		}
	}
	im := &image{id: newID(), name: body.Name, family: body.Family, status: "READY", arch: d.arch, labels: body.Labels,
		created: e.clock.Now(), sourceDisk: e.diskURL(d), sizeGB: d.sizeGB}
	op := e.newOp("", "insert", e.imageURL(im), im.id)
	if code, ok := e.failing("images.insert"); ok {
		return op.fail(http.StatusInternalServerError, code, "The operation failed."), nil
	}
	e.images[im.name] = im
	return op, nil
}

func (e *Emulator) setImageLabels(r *http.Request, _ string) (*operation, *apiError) {
	im := e.images[r.PathValue("name")]
	if im == nil {
		return nil, notFound("projects/" + e.Project + "/global/images/" + r.PathValue("name"))
	}
	var body gce.GlobalSetLabelsRequest
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.LabelFingerprint != fingerprint(im.labelFP) {
		return nil, &apiError{http.StatusPreconditionFailed, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed"}
	}
	if err := checkLabels(body.Labels); err != nil {
		return nil, err
	}
	op := e.newOp("", "setLabels", e.imageURL(im), im.id)
	im.labels, im.labelFP = body.Labels, im.labelFP+1
	return op, nil
}

func (e *Emulator) deleteImage(r *http.Request, _ string) (*operation, *apiError) {
	im := e.images[r.PathValue("name")]
	if im == nil {
		return nil, notFound("projects/" + e.Project + "/global/images/" + r.PathValue("name"))
	}
	op := e.newOp("", "delete", e.imageURL(im), im.id)
	delete(e.images, im.name)
	return op, nil
}

// on is a fault rule for requests whose path ends with suffix, such as
// "/instances" for inserts, by method.
func on(method, suffix string, times int, respond func(http.ResponseWriter, *http.Request), after bool) fault.Rule {
	return fault.Rule{Method: method, Times: times, After: after, Respond: respond,
		Match: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, suffix) }}
}

// LoseResponse makes the next times requests whose path ends with suffix do
// their work and then fail, as when a connection breaks after Compute
// Engine has accepted a change.
func (e *Emulator) LoseResponse(method, suffix string, times int) {
	e.faults.Add(on(method, suffix, times, JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error"), true))
}

// ServerError makes the next times requests whose path ends with suffix fail
// before doing anything.
func (e *Emulator) ServerError(method, suffix string, code, times int) {
	e.faults.Add(on(method, suffix, times, JSONError(code, "backendError", http.StatusText(code)), false))
}
