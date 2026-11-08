package fault_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lanescape/runners/emulators/fault"
)

func TestRules(t *testing.T) {
	var handled atomic.Int32
	var in fault.Injector
	srv := httptest.NewServer(in.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		io.WriteString(w, "ok")
	})))
	defer srv.Close()
	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	in.Add(fault.Rule{PathPrefix: "/a", Times: 2, Respond: fault.Status(503, nil, "")})
	if get("/a") != 503 || get("/b") != 200 || get("/a/x") != 503 || get("/a") != 200 {
		t.Fatal("a rule for two requests under /a")
	}
	if handled.Load() != 2 {
		t.Fatalf("handled %d, want 2", handled.Load())
	}

	in.Add(fault.Rule{Times: 1, After: true, Respond: fault.Status(503, nil, "")})
	if get("/c") != 503 || handled.Load() != 3 {
		t.Fatal("a lost response should do the work, then fail")
	}

	in.Add(fault.Rule{Method: http.MethodPost, Respond: fault.Drop})
	if get("/d") != 200 {
		t.Fatal("a POST rule matched a GET")
	}
	resp, err := http.Post(srv.URL+"/d", "text/plain", nil)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a dropped connection answered")
	}
	in.Reset()
	if get("/d") != 200 {
		t.Fatal("rules remain after Reset")
	}
}
