package storage

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SessionLifetime is how long a resumable upload's session URI stays open,
// as Cloud Storage keeps it: a week from when it was started.
const SessionLifetime = 7 * 24 * time.Hour

// statusResumeIncomplete is what Cloud Storage answers a chunk that isn't
// the upload's last, or a query of an upload still open.
const statusResumeIncomplete = 308

// statusClientClosed is what Cloud Storage answers the cancellation of an
// upload.
const statusClientClosed = 499

// session is a resumable upload, started with a signed URL.
type session struct {
	bucket, object string
	contentType    string
	conds          conds
	started        time.Time
	data           []byte
	// final is the object made, once the upload has finished.
	final *object
}

func errNoSession() *apiError {
	return &apiError{http.StatusNotFound, "notFound", "NoSuchUpload", "No such upload."}
}

func errSessionExpired() *apiError {
	return &apiError{http.StatusGone, "gone", "NoSuchUpload", "The upload session has expired."}
}

func newUploadID() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// startSession starts a resumable upload, as a signed POST with
// x-goog-resumable: start and no body does. Its preconditions, such as
// x-goog-if-generation-match: 0 for an object that mustn't exist yet, are
// checked when it finishes. It answers 201 with the session URI in
// Location: the object's URL, with the signed URL's query and an upload ID.
func (e *Emulator) startSession(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	c, err := headerConds(r.Header)
	if err != nil {
		writeXMLError(w, r, errInvalid(err.Error()))
		return
	}
	if body, _ := io.ReadAll(io.LimitReader(r.Body, 1)); len(body) > 0 {
		writeXMLError(w, r, errInvalid("a resumable upload starts with an empty body"))
		return
	}
	e.mu.Lock()
	if e.buckets[bucketName] == nil {
		e.mu.Unlock()
		writeXMLError(w, r, errNoBucket(bucketName))
		return
	}
	id := newUploadID()
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	e.sessions[id] = &session{bucket: bucketName, object: objectName, contentType: ct, conds: c, started: e.clock.Now()}
	e.mu.Unlock()
	u := *r.URL
	q := u.Query()
	q.Set("upload_id", id)
	loc := "http://" + r.Host + u.EscapedPath() + "?" + q.Encode()
	w.Header().Set("Location", loc)
	w.Header().Set("X-GUploader-UploadID", id)
	w.WriteHeader(http.StatusCreated)
}

// contentRange is a chunk's Content-Range: bytes first-last/total, with
// total -1 for "*", or bytes */total for a query, with first -1.
type contentRange struct {
	first, last, total int64
}

func parseContentRange(h string) (contentRange, bool) {
	spec, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return contentRange{}, false
	}
	rng, tot, ok := strings.Cut(spec, "/")
	if !ok {
		return contentRange{}, false
	}
	cr := contentRange{first: -1, last: -1, total: -1}
	if tot != "*" {
		n, err := strconv.ParseInt(tot, 10, 64)
		if err != nil || n < 0 {
			return contentRange{}, false
		}
		cr.total = n
	}
	if rng == "*" {
		return cr, true
	}
	a, b, ok := strings.Cut(rng, "-")
	if !ok {
		return contentRange{}, false
	}
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || first < 0 || last < first || (cr.total >= 0 && last >= cr.total) {
		return contentRange{}, false
	}
	cr.first, cr.last = first, last
	return cr, true
}

// serveSession takes a resumable upload's chunks, at its session URI, as
// Cloud Storage does. A PUT with Content-Range bytes first-last/total, or
// /* while the total isn't known, adds a chunk; one that starts before the
// bytes received so far has its overlap ignored, and one that starts after
// them is refused. A PUT with bytes */total or bytes */* and no body asks
// how far the upload has got. Until the last byte, the answer is 308 with
// the bytes persisted in Range, none at first; the last makes the object,
// subject to the preconditions it was started with, and answers 200, as
// does every query after. A DELETE cancels the upload, answering 499. A
// session is gone a week after it started.
func (e *Emulator) serveSession(w http.ResponseWriter, r *http.Request, bucketName, objectName, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[id]
	if s == nil || s.bucket != bucketName || s.object != objectName {
		writeXMLError(w, r, errNoSession())
		return
	}
	if e.clock.Now().Sub(s.started) >= SessionLifetime {
		delete(e.sessions, id)
		writeXMLError(w, r, errSessionExpired())
		return
	}
	switch r.Method {
	case http.MethodDelete:
		delete(e.sessions, id)
		w.WriteHeader(statusClientClosed)
		return
	case http.MethodPut:
	default:
		writeXMLError(w, r, &apiError{http.StatusMethodNotAllowed, "invalid", "MethodNotAllowed", "The specified method is not allowed against this resource."})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	if s.final != nil {
		e.writeFinished(w, s)
		return
	}
	received := int64(len(s.data))
	h := r.Header.Get("Content-Range")
	var cr contentRange
	switch {
	case h == "":
		// The whole object in one request.
		if received != 0 {
			writeXMLError(w, r, errInvalid("a request without Content-Range must hold the whole object"))
			return
		}
		cr = contentRange{first: 0, last: int64(len(body)) - 1, total: int64(len(body))}
		if len(body) == 0 {
			cr.first = -1
		}
	default:
		var ok bool
		if cr, ok = parseContentRange(h); !ok {
			writeXMLError(w, r, errInvalid("malformed Content-Range "+h))
			return
		}
	}
	if cr.first >= 0 {
		if int64(len(body)) != cr.last-cr.first+1 {
			writeXMLError(w, r, errInvalid("the body's length isn't the Content-Range's"))
			return
		}
		if cr.first > received {
			writeXMLError(w, r, errInvalid(fmt.Sprintf("the chunk starts at byte %d, and only %d have been received", cr.first, received)))
			return
		}
		if cr.last >= received {
			s.data = append(s.data, body[received-cr.first:]...)
		}
	} else if len(body) > 0 {
		writeXMLError(w, r, errInvalid("a query of an upload has no body"))
		return
	}
	received = int64(len(s.data))
	if cr.total >= 0 && received > cr.total {
		writeXMLError(w, r, errInvalid("more bytes than the upload's total"))
		return
	}
	if cr.total < 0 || received < cr.total {
		if received > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", received-1))
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(statusResumeIncomplete)
		return
	}
	// The last byte: the object is made, if its preconditions hold.
	b := e.buckets[s.bucket]
	if b == nil {
		delete(e.sessions, id)
		writeXMLError(w, r, errNoBucket(s.bucket))
		return
	}
	if err := s.conds.checkWrite(b.objects[s.object]); err != nil {
		delete(e.sessions, id)
		writeXMLError(w, r, err)
		return
	}
	s.final = e.write(b, s.object, s.data, s.contentType, objectMeta{})
	s.data = nil
	e.writeFinished(w, s)
}

// writeFinished answers for a finished upload with its object's
// generation, ETag and hashes, as Cloud Storage does.
func (e *Emulator) writeFinished(w http.ResponseWriter, s *session) {
	o := s.final
	sum := md5.Sum(o.data)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.Checksum(o.data, crc32.MakeTable(crc32.Castagnoli)))
	h := w.Header()
	h.Set("ETag", `"`+etag(o)+`"`)
	h.Set("X-Goog-Generation", strconv.FormatInt(o.generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(o.metageneration, 10))
	h.Set("X-Goog-Stored-Content-Length", strconv.Itoa(len(o.data)))
	h.Add("X-Goog-Hash", "crc32c="+base64.StdEncoding.EncodeToString(crc[:]))
	h.Add("X-Goog-Hash", "md5="+base64.StdEncoding.EncodeToString(sum[:]))
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}
