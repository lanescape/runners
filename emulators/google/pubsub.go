package google

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"google.golang.org/api/option"

	"github.com/lanescape/runners/emulators/fault"
)

// PubSub emulates Pub/Sub's REST publishing, as
// google.golang.org/api/pubsub/v1 calls it: topics that keep every message
// published to them, for tests to read and push on.
type PubSub struct {
	// URL is the emulator's base address, with no trailing slash.
	URL string

	srv    *httptest.Server
	faults fault.Injector
	clock  clockwork.Clock

	mu     sync.Mutex
	topics map[string][]Message
	nextID int64
}

// Message is a published message.
type Message struct {
	ID          string
	Data        []byte
	Attributes  map[string]string
	PublishTime time.Time
}

// PubSubOption configures a PubSub.
type PubSubOption func(*PubSub)

// WithPubSubClock sets the clock that stamps messages' publish times.
func WithPubSubClock(c clockwork.Clock) PubSubOption { return func(p *PubSub) { p.clock = c } }

// NewPubSub starts a Pub/Sub emulator. Close it when done.
func NewPubSub(opts ...PubSubOption) *PubSub {
	p := &PubSub{clock: clockwork.NewRealClock(), topics: map[string][]Message{}, nextID: 1000}
	for _, o := range opts {
		o(p)
	}
	p.srv = httptest.NewServer(p.faults.Wrap(http.HandlerFunc(p.serve)))
	p.URL = p.srv.URL
	return p
}

// Close stops the emulator.
func (p *PubSub) Close() { p.srv.Close() }

// Faults is where tests make requests fail.
func (p *PubSub) Faults() *fault.Injector { return &p.faults }

// ClientOptions are what google.golang.org/api/pubsub/v1 needs to talk to
// the emulator instead of Google.
func (p *PubSub) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(p.URL + "/"), option.WithoutAuthentication()}
}

// CreateTopic makes a topic, by its full name, projects/<project>/topics/<topic>.
func (p *PubSub) CreateTopic(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.topics[name]; !ok {
		p.topics[name] = nil
	}
}

// Messages are those published to a topic, in order.
func (p *PubSub) Messages(topic string) []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.topics[topic])
}

var publishPath = regexp.MustCompile(`^/v1/(projects/[^/]+/topics/[^/:]+):publish$`)

func (p *PubSub) serve(w http.ResponseWriter, r *http.Request) {
	m := publishPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost {
		writeGoogleError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return
	}
	var req struct {
		Messages []struct {
			Data       []byte            `json:"data"`
			Attributes map[string]string `json:"attributes"`
		} `json:"messages"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid JSON payload received.")
		return
	}
	if len(req.Messages) == 0 {
		writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "No messages to publish.")
		return
	}
	for _, msg := range req.Messages {
		if len(msg.Data) == 0 && len(msg.Attributes) == 0 {
			writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Some messages are empty.")
			return
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	msgs, ok := p.topics[m[1]]
	if !ok {
		writeGoogleError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("Resource not found (resource=%s).", m[1]))
		return
	}
	var ids []string
	for _, msg := range req.Messages {
		p.nextID++
		id := strconv.FormatInt(p.nextID, 10)
		attrs := map[string]string{}
		for k, v := range msg.Attributes {
			attrs[k] = v
		}
		msgs = append(msgs, Message{ID: id, Data: msg.Data, Attributes: attrs, PublishTime: p.clock.Now()})
		ids = append(ids, id)
	}
	p.topics[m[1]] = msgs
	writeJSON(w, http.StatusOK, map[string]any{"messageIds": ids})
}
