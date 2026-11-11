// Package logging reads the service's machines' boot logs from Cloud
// Logging (specs/0002-machines.md, A machine's life): Compute Engine sends
// a machine's serial console there, and serves a stopped or deleted
// machine's console from nowhere else. The service reads them through the
// view of a log bucket of its own, which holds only its machines' serial
// console entries, routed there by a sink; bootstrap/ makes them and lets
// the service's account alone read the view.
package logging

import (
	"context"
	"fmt"
	"strings"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
)

// View is the log bucket, and its view, the service reads its machines'
// boot logs through, which bootstrap/ makes.
const View = "lanescape-runners-serial"

// Client reads boot logs.
type Client struct {
	svc     *logging.Service
	project string
	view    string
}

// New connects to Cloud Logging for a project's machines, through view.
func New(ctx context.Context, project, view string, opts ...option.ClientOption) (*Client, error) {
	svc, err := logging.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc, project: project, view: view}, nil
}

// Serial is the serial console output an instance wrote, by its numeric
// ID, in order, up to limit bytes.
func (c *Client) Serial(ctx context.Context, instanceID uint64, limit int) (string, error) {
	req := &logging.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s/locations/global/buckets/%s/views/%s", c.project, c.view, c.view)},
		Filter: fmt.Sprintf(`resource.type="gce_instance" AND resource.labels.instance_id="%d" AND logName="projects/%s/logs/serialconsole.googleapis.com%%2Fserial_port_1_output"`,
			instanceID, c.project),
		OrderBy:  "timestamp asc",
		PageSize: 1000,
	}
	var b strings.Builder
	for {
		resp, err := c.svc.Entries.List(req).Context(ctx).Do()
		if err != nil {
			return "", err
		}
		for _, en := range resp.Entries {
			if b.Len()+len(en.TextPayload) > limit {
				b.WriteString(en.TextPayload[:max(0, limit-b.Len())])
				return b.String(), nil
			}
			b.WriteString(en.TextPayload)
			if !strings.HasSuffix(en.TextPayload, "\n") {
				b.WriteString("\n")
			}
		}
		if resp.NextPageToken == "" {
			return b.String(), nil
		}
		req.PageToken = resp.NextPageToken
	}
}
