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
