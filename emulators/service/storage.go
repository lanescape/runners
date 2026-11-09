package service

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// grant is what a URL the emulator hands out allows: one method on one
// object, until it expires. The URL's token is its credential.
type grant struct {
	method  string
	object  string
	expires time.Time
}

// session is a resumable upload, started through a grant, which takes
// bytes at its session URI for a week.
type session struct {
	object  string
	data    []byte
	expires time.Time
	done    bool
}

// grant hands out a URL for one method on one object.
func (e *Emulator) grant(method, object string, expires time.Time) string {
	tok := randomID()
	e.grants[tok] = &grant{method: method, object: object, expires: expires}
	return e.url + "/storage/" + tok
}

func storageError(w http.ResponseWriter, code int, what string) {
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><Error><Code>%s</Code></Error>", what)
}

// storage serves the URLs the emulator hands out, as Cloud Storage serves
// signed URLs: a GET of an entry's object, whole or ranged; a POST that
// starts a resumable upload, with exactly the headers given; and the
// upload's session URI, which takes chunks with Content-Range and answers
// how far it has got.
func (e *Emulator) storage(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/storage/")
	if sid, ok := strings.CutPrefix(rest, "session/"); ok {
		e.upload(w, r, sid)
		return
	}
	e.mu.Lock()
	g := e.grants[rest]
	e.mu.Unlock()
	switch {
	case g == nil:
		storageError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	case !e.now().Before(g.expires):
		storageError(w, http.StatusBadRequest, "ExpiredToken")
		return
	case r.Method != g.method:
		storageError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	if g.method == http.MethodPost {
		e.start(w, r, g)
		return
	}
	e.mu.Lock()
	data, ok := e.objects[g.object]
	e.mu.Unlock()
	if !ok {
		storageError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := parseRange(rng, int64(len(data)))
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			storageError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// start begins a resumable upload, only with the headers the grant was
// signed with. Its precondition, that the object doesn't exist yet, is
// checked when the upload finishes, as Cloud Storage checks it.
func (e *Emulator) start(w http.ResponseWriter, r *http.Request, g *grant) {
	if r.Header.Get("x-goog-resumable") != "start" || r.Header.Get("x-goog-if-generation-match") != "0" {
		storageError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sid := randomID()
	e.sessions[sid] = &session{object: g.object, expires: e.now().Add(SessionFor)}
	w.Header().Set("Location", e.url+"/storage/session/"+sid)
	w.WriteHeader(http.StatusCreated)
}

// upload takes a chunk at a session URI, as Cloud Storage's resumable
// uploads do: 308 with the Range received until the last byte, then 200.
func (e *Emulator) upload(w http.ResponseWriter, r *http.Request, sid string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[sid]
	switch {
	case s == nil:
		storageError(w, http.StatusNotFound, "NoSuchUpload")
		return
	case !e.now().Before(s.expires):
		delete(e.sessions, sid)
		storageError(w, http.StatusGone, "NoSuchUpload")
		return
	case r.Method == http.MethodDelete:
		delete(e.sessions, sid)
		w.WriteHeader(499)
		return
	case r.Method != http.MethodPut:
		storageError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	case s.done:
		w.WriteHeader(http.StatusOK)
		return
	}
	cr := r.Header.Get("Content-Range")
	if cr == "" && len(s.data) > 0 {
		// A whole object, sent once part of one has been.
		storageError(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	if cr == "" {
		// The whole object, in one request.
		cr = fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body))
		if len(body) == 0 {
			cr = "bytes */0"
		}
	}
	spec, ok := strings.CutPrefix(cr, "bytes ")
	if !ok {
		storageError(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	span, total, _ := strings.Cut(spec, "/")
	have := int64(len(s.data))
	if span != "*" {
		a, b, ok := strings.Cut(span, "-")
		start, err1 := strconv.ParseInt(a, 10, 64)
		end, err2 := strconv.ParseInt(b, 10, 64)
		if !ok || err1 != nil || err2 != nil || end < start || end-start+1 != int64(len(body)) || start > have {
			storageError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		// Bytes already received are ignored.
		if end >= have {
			s.data = append(s.data, body[have-start:]...)
		}
	}
	if total != "*" {
		n, err := strconv.ParseInt(total, 10, 64)
		if err != nil || n < int64(len(s.data)) {
			storageError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		if n == int64(len(s.data)) {
			if _, exists := e.objects[s.object]; exists {
				delete(e.sessions, sid)
				storageError(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
			e.objects[s.object] = s.data
			s.done = true
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if len(s.data) > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.data)-1))
	}
	w.WriteHeader(http.StatusPermanentRedirect)
}

// parseRange reads a single "bytes=" range: a-b, a- or -n.
func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 || size == 0 {
			return 0, 0, false
		}
		return max(size-n, 0), size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if b == "" {
		return start, size - 1, true
	}
	end, err := strconv.ParseInt(b, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, min(end, size-1), true
}
