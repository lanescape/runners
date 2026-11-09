package google_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/pubsub/v1"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/google"
)

const topic = "projects/lanescape-dev/topics/lanescape-runners-webhooks"

func TestPublish(t *testing.T) {
	emu := google.NewPubSub()
	t.Cleanup(emu.Close)
	emu.CreateTopic(topic)
	svc, err := pubsub.NewService(context.Background(), emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(name string, msgs ...*pubsub.PubsubMessage) (*pubsub.PublishResponse, error) {
		return svc.Projects.Topics.Publish(name, &pubsub.PublishRequest{Messages: msgs}).Do()
	}
	res, err := publish(topic,
		&pubsub.PubsubMessage{Data: base64.StdEncoding.EncodeToString([]byte(`{"action":"closed"}`)), Attributes: map[string]string{"event": "pull_request"}},
		&pubsub.PubsubMessage{Attributes: map[string]string{"only": "attributes"}})
	if err != nil {
		t.Fatal(err)
	}
	got := emu.Messages(topic)
	if len(res.MessageIds) != 2 || len(got) != 2 || got[0].ID != res.MessageIds[0] || got[1].ID != res.MessageIds[1] {
		t.Fatalf("published %v, emulator has %+v", res.MessageIds, got)
	}
	if string(got[0].Data) != `{"action":"closed"}` || got[0].Attributes["event"] != "pull_request" || got[0].PublishTime.IsZero() {
		t.Fatalf("message = %+v", got[0])
	}
	code := func(err error) int {
		var ge *googleapi.Error
		if errors.As(err, &ge) {
			return ge.Code
		}
		return 0
	}
	if _, err := publish("projects/lanescape-dev/topics/missing", &pubsub.PubsubMessage{Data: "eA=="}); code(err) != http.StatusNotFound {
		t.Fatalf("publishing to a missing topic: %v, want 404", err)
	}
	if _, err := publish(topic, &pubsub.PubsubMessage{}); code(err) != http.StatusBadRequest {
		t.Fatalf("publishing an empty message: %v, want 400", err)
	}
	emu.Faults().Add(fault.Rule{Times: 1, Respond: google.GoogleError(http.StatusServiceUnavailable, "UNAVAILABLE", "try again")})
	if _, err := publish(topic, &pubsub.PubsubMessage{Data: "eA=="}); code(err) != http.StatusServiceUnavailable {
		t.Fatalf("publishing with a fault: %v, want 503", err)
	}
	if len(emu.Messages(topic)) != 2 {
		t.Fatal("a refused message was kept")
	}
}
