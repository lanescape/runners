// Package pubsub publishes the webhook's checked deliveries to Pub/Sub
// (specs/0001-architecture.md, Components): each delivery's body as a
// message's data, with its event as the attribute "event", for the push
// subscription to deliver to the reconcile endpoint.
package pubsub

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	ps "google.golang.org/api/pubsub/v1"

	"github.com/lanescape/runners/internal/server"
)

// topicName is a topic's full name.
var topicName = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/topics/[A-Za-z][A-Za-z0-9._~%+-]{2,254}$`)

// Publisher publishes deliveries to one topic. It implements
// server.Publisher.
type Publisher struct {
	topic string
	svc   *ps.Service
	// Attempts is how many times a delivery is tried while Pub/Sub's
	// answers confirm nothing, within the caller's context.
	Attempts int
	// Backoff is the wait before the second try, doubled for each after.
	Backoff time.Duration
}

// New returns a publisher to topic, a full name, projects/<project>/topics/<topic>.
// opts are passed to the Pub/Sub client, such as an emulator's endpoint in
// tests.
func New(ctx context.Context, topic string, opts ...option.ClientOption) (*Publisher, error) {
	if !topicName.MatchString(topic) {
		return nil, fmt.Errorf("pubsub: %q isn't a topic's full name", topic)
	}
	svc, err := ps.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("pubsub client: %w", err)
	}
	return &Publisher{topic: topic, svc: svc, Attempts: 3, Backoff: 200 * time.Millisecond}, nil
}

// Publish implements server.Publisher: it publishes the delivery's body,
// with its event as the attribute "event", trying again while Pub/Sub's
// answers confirm nothing.
func (p *Publisher) Publish(ctx context.Context, d server.Delivery) error {
	if d.Event == "" || len(d.Body) == 0 {
		return errors.New("pubsub: a delivery needs its event and body")
	}
	req := &ps.PublishRequest{Messages: []*ps.PubsubMessage{{
		Data:       base64.StdEncoding.EncodeToString(d.Body),
		Attributes: map[string]string{"event": d.Event},
	}}}
	wait := p.Backoff
	var err error
	for attempt := range max(p.Attempts, 1) {
		if attempt > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return errors.Join(err, ctx.Err())
			case <-t.C:
			}
			wait *= 2
		}
		var res *ps.PublishResponse
		res, err = p.svc.Projects.Topics.Publish(p.topic, req).Context(ctx).Do()
		if err == nil {
			if len(res.MessageIds) != 1 {
				return fmt.Errorf("pubsub: published %d messages, not 1", len(res.MessageIds))
			}
			return nil
		}
		if !retryable(err) {
			return err
		}
	}
	return err
}

// retryable reports whether a failure to publish confirms nothing: Pub/Sub
// couldn't be reached, or answered with an error it says to try again on.
func retryable(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return true
	}
	switch ge.Code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusRequestTimeout, 499:
		return true
	}
	return false
}
