package storage

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strconv"
)

// copyObject copies an object within the emulator, as the JSON API's
// rewrite and copy calls do: from a source, at sourceGeneration if given,
// to a destination, subject to the destination's preconditions
// (ifGenerationMatch=0 for one that mustn't exist yet) and the source's
// (ifSourceGenerationMatch and the like). The destination takes the body's
// content type and metadata, or the source's if the body sets none. A
// rewrite is always done in one call.
func (e *Emulator) copyObject(w http.ResponseWriter, r *http.Request, src, dst string, rewrite bool) {
	srcBucket, srcName, _, hasSrc, err1 := splitObjectPath(src)
	dstBucket, dstName, _, hasDst, err2 := splitObjectPath(dst)
	if err1 != nil || err2 != nil || !hasSrc || !hasDst {
		writeJSONError(w, &apiError{http.StatusNotFound, "notFound", "NotFound", "Not Found"})
		return
	}
	q := r.URL.Query()
	dc := queryConds(q)
	var sc conds
	var errs []error
	get := func(name string) *int64 {
		v, err := optInt(q.Get(name))
		errs = append(errs, err)
		return v
	}
	sc.generation = get("sourceGeneration")
	sc.ifGenerationMatch = get("ifSourceGenerationMatch")
	sc.ifGenerationNotMatch = get("ifSourceGenerationNotMatch")
	sc.ifMetagenerationMatch = get("ifSourceMetagenerationMatch")
	sc.ifMetagenerationNotMat = get("ifSourceMetagenerationNotMatch")
	for _, err := range append(errs, dc.err) {
		if err != nil {
			writeJSONError(w, errInvalid(err.Error()))
			return
		}
	}
	var meta objectMeta
	if body, err := io.ReadAll(r.Body); err == nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &meta); err != nil {
			writeJSONError(w, errInvalid("the destination's metadata isn't JSON"))
			return
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	_, so, err := e.lookup(srcBucket, srcName)
	if err == nil {
		err = sc.checkExisting(so, false)
	}
	if err != nil {
		writeJSONError(w, err)
		return
	}
	db := e.buckets[dstBucket]
	if db == nil {
		writeJSONError(w, errNoBucket(dstBucket))
		return
	}
	if err := dc.checkWrite(db.objects[dstName]); err != nil {
		writeJSONError(w, err)
		return
	}
	ct := meta.ContentType
	if ct == "" {
		ct = so.contentType
		meta.ContentEncoding, meta.CacheControl = so.contentEncoding, so.cacheControl
		if meta.Metadata == nil {
			meta.Metadata = maps.Clone(so.metadata)
		}
	}
	o := e.write(db, dstName, bytes.Clone(so.data), ct, meta)
	res := e.resource(db, o)
	if !rewrite {
		writeJSON(w, http.StatusOK, res)
		return
	}
	size := strconv.Itoa(len(o.data))
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": "storage#rewriteResponse", "totalBytesRewritten": size, "objectSize": size, "done": true, "resource": res,
	})
}
