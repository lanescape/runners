package compute

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The metadata server, one per instance under its own base address, as a
// machine reaches http://metadata.google.internal. Only a running machine's
// is served.

func (e *Emulator) metadataRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{id}/computeMetadata/v1/{path...}", e.metadata)
	mux.HandleFunc("POST /{id}/_emulator/poweroff", e.poweroff)
	mux.HandleFunc("POST /{id}/_emulator/serial", e.serial)
	return mux
}

// guest finds the running instance a metadata request comes from.
func (e *Emulator) guest(w http.ResponseWriter, r *http.Request) *instance {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	e.tick()
	for _, in := range e.instances {
		if in.id == id && e.running(in) {
			return in
		}
	}
	http.Error(w, "no such machine is running", http.StatusServiceUnavailable)
	return nil
}

// accountID is a service account's stable numeric ID, as Google gives one.
func accountID(email string) string {
	sum := sha256.Sum256([]byte(email))
	return strconv.FormatUint(binary.BigEndian.Uint64(sum[:8])>>1, 10)
}

// IdentityClaims are the claims of an instance's identity token for
// audience, in the full format if full, which tests may change before
// minting.
func (e *Emulator) IdentityClaims(zone, name, audience string, full bool) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.instances[zone+"/"+name]
	if in == nil {
		panic("compute emulator: no instance " + zone + "/" + name)
	}
	return e.identityClaims(in, audience, full)
}

func (e *Emulator) identityClaims(in *instance, audience string, full bool) map[string]any {
	id := accountID(in.serviceAccount)
	c := map[string]any{"aud": audience, "azp": id, "sub": id, "email": in.serviceAccount, "email_verified": true}
	if full {
		c["google"] = map[string]any{"compute_engine": map[string]any{
			"project_id":                  e.Project,
			"project_number":              e.ProjectNumber,
			"zone":                        in.zone,
			"instance_id":                 strconv.FormatUint(in.id, 10),
			"instance_name":               in.name,
			"instance_creation_timestamp": in.created.Unix(),
		}}
	}
	return c
}

func (e *Emulator) metadata(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Metadata-Flavor") != "Google" || r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
		return
	}
	e.mu.Lock()
	in := e.guest(w, r)
	if in == nil {
		e.mu.Unlock()
		return
	}
	path := r.PathValue("path")
	text := func(s string) {
		e.mu.Unlock()
		w.Header().Set("Metadata-Flavor", "Google")
		w.Header().Set("Content-Type", "application/text")
		_, _ = io.WriteString(w, s)
	}
	missing := func() {
		e.mu.Unlock()
		http.Error(w, "Not Found", http.StatusNotFound)
	}
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(path, "instance/guest-attributes/"):
		key := strings.TrimPrefix(path, "instance/guest-attributes/")
		ns, k, ok := strings.Cut(key, "/")
		enabled := false
		for _, it := range in.metadata {
			if it.key == "enable-guest-attributes" && strings.EqualFold(it.value, "true") {
				enabled = true
			}
		}
		if !ok || ns == "" || k == "" || strings.Contains(k, "/") {
			e.mu.Unlock()
			http.Error(w, "guest attributes are namespace/key", http.StatusBadRequest)
			return
		}
		if !enabled {
			e.mu.Unlock()
			http.Error(w, "Guest attributes endpoint access is disabled.", http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		in.guest[ns+"/"+k] = string(b)
		text("")
	case r.Method != http.MethodGet:
		e.mu.Unlock()
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	case path == "instance/id":
		text(strconv.FormatUint(in.id, 10))
	case path == "instance/name":
		text(in.name)
	case path == "instance/zone":
		text(fmt.Sprintf("projects/%d/zones/%s", e.ProjectNumber, in.zone))
	case path == "project/project-id":
		text(e.Project)
	case path == "project/numeric-project-id":
		text(strconv.FormatInt(e.ProjectNumber, 10))
	case path == "instance/attributes/":
		var keys []string
		for _, it := range in.metadata {
			keys = append(keys, it.key)
		}
		text(strings.Join(keys, "\n") + "\n")
	case strings.HasPrefix(path, "instance/attributes/"):
		key := strings.TrimPrefix(path, "instance/attributes/")
		for _, it := range in.metadata {
			if it.key == key {
				text(it.value)
				return
			}
		}
		missing()
	case strings.HasPrefix(path, "instance/guest-attributes/"):
		v, ok := in.guest[strings.TrimPrefix(path, "instance/guest-attributes/")]
		if !ok {
			missing()
			return
		}
		text(v)
	case path == "instance/service-accounts/default/email":
		if in.serviceAccount == "" {
			missing()
			return
		}
		text(in.serviceAccount)
	case path == "instance/service-accounts/default/identity":
		if in.serviceAccount == "" {
			missing()
			return
		}
		aud := r.URL.Query().Get("audience")
		if aud == "" {
			e.mu.Unlock()
			http.Error(w, "non-empty audience parameter required", http.StatusBadRequest)
			return
		}
		c := e.identityClaims(in, aud, r.URL.Query().Get("format") == "full")
		e.mu.Unlock()
		tok := e.Identity.Mint(c)
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = io.WriteString(w, tok)
	default:
		missing()
	}
}

// poweroff is the guest shutting itself down, in place of systemctl
// poweroff.
func (e *Emulator) poweroff(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.guest(w, r)
	if in == nil {
		return
	}
	e.systemOp(in, "compute.instances.guestTerminate")
	e.halt(in, "TERMINATED")
	w.WriteHeader(http.StatusNoContent)
}

// serial is the guest writing to its serial console.
func (e *Emulator) serial(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	e.mu.Lock()
	defer e.mu.Unlock()
	in := e.guest(w, r)
	if in == nil {
		return
	}
	in.serial.Write(b)
	w.WriteHeader(http.StatusNoContent)
}
