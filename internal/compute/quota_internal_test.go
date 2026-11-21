package compute

import (
	"testing"

	gce "google.golang.org/api/compute/v1"
)

// An operation's quota error names its quota in its details, which are
// what's read when its message doesn't name one.
func TestAnOperationsQuotaIsNamedByItsDetails(t *testing.T) {
	for _, tc := range []struct {
		name, message, metric, want string
	}{
		{"details alone", "The request exceeded a quota.", "compute.googleapis.com/cpus_all_regions", "CPUS_ALL_REGIONS"},
		{"details over the message", "Quota 'CPUS' exceeded.  Limit: 8.0 in region r.", "compute.googleapis.com/cpus_per_vm_family", "CPUS_PER_VM_FAMILY"},
		{"the message alone", "Quota 'IN_USE_ADDRESSES' exceeded.  Limit: 8.0 in region r.", "", "IN_USE_ADDRESSES"},
		{"neither", "The request exceeded a quota.", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &gce.OperationErrorErrors{Code: "QUOTA_EXCEEDED", Message: tc.message}
			if tc.metric != "" {
				e.ErrorDetails = []*gce.OperationErrorErrorsErrorDetails{{QuotaInfo: &gce.QuotaExceededInfo{MetricName: tc.metric}}}
			}
			err := opError(&gce.Operation{Error: &gce.OperationError{Errors: []*gce.OperationErrorErrors{e}}})
			c, ok := err.(*CapacityError)
			if !ok || c.Stockout || c.Quota != tc.want {
				t.Fatalf("%#v, want a quota error naming %q", err, tc.want)
			}
		})
	}
}

// A quota error gives the quota's whole limit, from the details that name the
// quota, or else from its message, so that a job asking for more than all of
// it is known; an error that gives none has no limit.
func TestAQuotaErrorGivesItsLimit(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		details       *gce.QuotaExceededInfo
		limit         float64
		has           bool
	}{
		{"the message", "Quota 'CPUS_PER_VM_FAMILY' exceeded.  Limit: 48.0 in region r.", nil, 48, true},
		{"the details over the message", "Quota 'CPUS' exceeded.  Limit: 8.0 in region r.",
			&gce.QuotaExceededInfo{MetricName: "compute.googleapis.com/cpus_per_vm_family", LimitName: "CPUS-PER-VM-FAMILY-per-project-region", Limit: 48}, 48, true},
		{"a limit of nothing", "Quota 'CPUS_PER_VM_FAMILY' exceeded.  Limit: 0.0 in region r.", nil, 0, true},
		{"details without their limit, the message another quota's", "Quota 'CPUS' exceeded.  Limit: 96.0 in region r.",
			&gce.QuotaExceededInfo{MetricName: "compute.googleapis.com/cpus_per_vm_family", LimitName: "CPUS-PER-VM-FAMILY-per-project-region"}, 0, false},
		{"details without their limit, the message the same quota's", "Quota 'CPUS_PER_VM_FAMILY' exceeded.  Limit: 64.0 in region r.",
			&gce.QuotaExceededInfo{MetricName: "compute.googleapis.com/cpus_per_vm_family", LimitName: "CPUS-PER-VM-FAMILY-per-project-region"}, 64, true},
		{"neither", "The request exceeded a quota.", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &gce.OperationErrorErrors{Code: "QUOTA_EXCEEDED", Message: tc.message}
			if tc.details != nil {
				e.ErrorDetails = []*gce.OperationErrorErrorsErrorDetails{{QuotaInfo: tc.details}}
			}
			c, ok := opError(&gce.Operation{Error: &gce.OperationError{Errors: []*gce.OperationErrorErrors{e}}}).(*CapacityError)
			if !ok || c.Limit != tc.limit || c.HasLimit != tc.has {
				t.Fatalf("%#v, want the limit %v (%v)", c, tc.limit, tc.has)
			}
			if limit, has := quotaLimit(tc.message); tc.details == nil && (limit != tc.limit || has != tc.has) {
				t.Fatalf("the message's limit is %v (%v), want %v (%v)", limit, has, tc.limit, tc.has)
			}
		})
	}
}
