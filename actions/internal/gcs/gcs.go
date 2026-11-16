// Package gcs uploads and downloads a cache's objects with Cloud Storage's
// JSON API, signed with the short-lived, downscoped access token the runner
// service hands a job for them (specs/0007-actions.md, save-cache and
// cache). Uploads are resumable and streamed; downloads are pinned to a
// generation and checked against the object's CRC32C as they stream.
// Tokens are never logged, nor put in an error.
package gcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/lanescape/runners/actions/internal/backoff"
)

// DefaultURL is Cloud Storage's own address.
const DefaultURL = "https://storage.googleapis.com"

// Tokens gives the access token for a cache's objects, and gets a new one
// when one is refused as expired.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	// Expired says tok was refused: the next Token is a new one.
	Expired(tok string)
}

// Client calls Cloud Storage.
type Client struct {
	// URL is Cloud Storage's address; empty means DefaultURL.
	URL string
	// HTTP is the client to use; nil means one with no overall timeout,
	// since uploads and downloads stream for as long as they take.
	HTTP *http.Client
	// Retry is how failed requests are retried; zero means DefaultRetry.
	Retry backoff.Policy
	// ChunkSize is how much of an upload each request sends, a multiple
	// of 256 KiB; zero means DefaultChunkSize.
	ChunkSize int
	// Logf, if set, is told of each retry.
	Logf func(format string, args ...any)
}

// DefaultRetry is how a request that may succeed later is retried: eight
// attempts in all, the waits doubling from a second to half a minute.
var DefaultRetry = backoff.Policy{Attempts: 8, First: time.Second, Max: 30 * time.Second}

// DefaultChunkSize is how much of an upload each request sends.
const DefaultChunkSize = 8 << 20

// chunkUnit is what Cloud Storage needs every chunk but the last to be a
// multiple of.
const chunkUnit = 256 << 10

// Object is an object of a cache: its full name, size, CRC32C and
// generation.
type Object struct {
	Name       string
	Size       int64
	CRC32C     uint32
	Generation int64
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ParseCRC32C reads a CRC32C as Cloud Storage writes it: the big-endian
// value, in base64.
func ParseCRC32C(s string) (uint32, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 4 {
		return 0, fmt.Errorf("%q isn't a CRC32C as Cloud Storage writes it", s)
	}
	return binary.BigEndian.Uint32(b), nil
}

// FormatCRC32C writes a CRC32C as Cloud Storage does.
func FormatCRC32C(v uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return base64.StdEncoding.EncodeToString(b[:])
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// ValidBucket is whether name is a bucket's name.
func ValidBucket(name string) bool { return bucketName.MatchString(name) }

// ValidObject is whether name is an object's name Cloud Storage takes.
func ValidObject(name string) bool {
	if name == "" || len(name) > 1024 || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	return !strings.ContainsRune(name, unicode.ReplacementChar)
}

func (c *Client) base() string {
	if c.URL == "" {
		return DefaultURL
	}
	return strings.TrimRight(c.URL, "/")
}

func (c *Client) policy() backoff.Policy {
	if c.Retry.Attempts <= 0 {
		return DefaultRetry
	}
	return c.Retry
}

// transport is the default client's: an upload or download streams for
// as long as it takes, but an answer that doesn't start in two minutes is
// given up on. Every stream has a connection of its own, over HTTP/1.1,
// and may keep it: the dozens a cache streams at once would share one
// over HTTP/2.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	var p http.Protocols
	p.SetHTTP1(true)
	t.Protocols = &p
	t.ResponseHeaderTimeout = 2 * time.Minute
	t.MaxIdleConnsPerHost = 256
	return t
}()

// chunkTimeout is how long a chunk of an upload may take.
const chunkTimeout = 5 * time.Minute

func (c *Client) http() *http.Client {
	hc := &http.Client{Transport: transport}
	if c.HTTP != nil {
		copied := *c.HTTP
		hc = &copied
	}
	// Nothing Cloud Storage answers is a redirect: following one would
	// send the token elsewhere.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

// statusError is an answer that wasn't what was asked for.
type statusError struct {
	code    int
	message string
}

func (e *statusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("Cloud Storage answered %d", e.code)
	}
	return fmt.Sprintf("Cloud Storage answered %d: %s", e.code, e.message)
}

// retryable is whether an error may go away if the request is made again.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusRequestTimeout || se.code == http.StatusTooManyRequests || se.code >= 500
	}
	var ne *netError
	return errors.As(err, &ne) || errors.Is(err, errStalled)
}

// errStalled is an upload that kept nothing of what was sent.
var errStalled = errors.New("Cloud Storage kept nothing of what was sent")

// netError is a request that got no answer.
type netError struct{ op string }

func (e *netError) Error() string { return "Cloud Storage couldn't be reached to " + e.op }

// answerError reads an error answer's message, on one line, with tok
// nowhere in it.
func answerError(resp *http.Response, tok string) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(data, &body) == nil {
		msg = body.Error.Message
	}
	if tok != "" {
		msg = strings.ReplaceAll(msg, tok, "***")
	}
	msg = strings.Join(strings.FieldsFunc(msg, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return &statusError{code: resp.StatusCode, message: msg}
}

// attempts runs try until it succeeds, fails for good, or the retries are
// spent. A 401 gets a new token and is tried again, as is anything
// retryable, after a wait.
func (c *Client) attempts(ctx context.Context, tokens Tokens, what string, try func(tok string) error) error {
	p := c.policy()
	refreshed := false
	for n := 1; ; n++ {
		tok := ""
		if tokens != nil {
			var err error
			if tok, err = tokens.Token(ctx); err != nil {
				return fmt.Errorf("%s: a token for the cache's objects: %w", what, err)
			}
		}
		err := try(tok)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusUnauthorized && tokens != nil && !refreshed {
			// An expired token gets one new one, at once.
			tokens.Expired(tok)
			refreshed = true
			continue
		}
		if !retryable(err) || n >= p.Attempts {
			return fmt.Errorf("%s: %w", what, err)
		}
		refreshed = false
		wait := p.Wait(n, 0)
		if c.Logf != nil {
			c.Logf("%s: %v; trying again in %s.", what, err, wait.Round(time.Millisecond))
		}
		if err := p.Pause(ctx, wait); err != nil {
			return err
		}
	}
}

// Upload uploads what r gives as the object name in bucket, creating it,
// never replacing one, with a resumable upload of ChunkSize at a time,
// and checks Cloud Storage's size and CRC32C of it against what was sent.
func (c *Client) Upload(ctx context.Context, tokens Tokens, bucket, name string, r io.Reader) (Object, error) {
	what := "uploading " + name
	size := c.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	if size%chunkUnit != 0 {
		return Object{}, fmt.Errorf("%s: a chunk of %d bytes isn't a multiple of 256 KiB", what, size)
	}
	session, err := c.start(ctx, tokens, bucket, name)
	if err != nil {
		return Object{}, err
	}

	// One chunk is read while the one before it is sent.
	type chunk struct {
		data []byte
		last bool
		err  error
	}
	chunks := make(chan chunk)
	free := make(chan []byte, 2)
	free <- make([]byte, size)
	free <- make([]byte, size)
	// The reader isn't waited for: it may be blocked reading r until
	// whoever writes it stops, once Upload has returned.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		br := &peeker{r: r}
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			n, err := io.ReadFull(br, buf)
			ch := chunk{data: buf[:n]}
			switch {
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				ch.last = true
			case err != nil:
				ch.err = err
			default:
				// A full chunk is the last if nothing follows it.
				more, perr := br.more()
				ch.last, ch.err = !more, perr
			}
			select {
			case chunks <- ch:
			case <-stop:
				return
			}
			if ch.last || ch.err != nil {
				return
			}
		}
	}()

	crc := crc32.New(castagnoli)
	var sent int64
	for {
		var ch chunk
		select {
		case ch = <-chunks:
		case <-ctx.Done():
			return Object{}, ctx.Err()
		}
		if ch.err != nil {
			return Object{}, fmt.Errorf("%s: %w", what, ch.err)
		}
		crc.Write(ch.data)
		total := int64(-1)
		if ch.last {
			total = sent + int64(len(ch.data))
		}
		res, err := c.put(ctx, session, sent, ch.data, total)
		if err != nil {
			return Object{}, fmt.Errorf("%s: %w", what, err)
		}
		sent += int64(len(ch.data))
		if !ch.last {
			free <- ch.data[:cap(ch.data)]
			continue
		}
		got, err := res.object()
		if err != nil {
			return Object{}, fmt.Errorf("%s: %w", what, err)
		}
		if got.Name != name || got.Size != sent || got.CRC32C != crc.Sum32() {
			return Object{}, fmt.Errorf("%s: Cloud Storage has an object of %d bytes, CRC32C %s, where %d bytes, CRC32C %s, were sent to it",
				what, got.Size, FormatCRC32C(got.CRC32C), sent, FormatCRC32C(crc.Sum32()))
		}
		return got, nil
	}
}

// peeker reads r, telling whether anything follows what's been read.
type peeker struct {
	r    io.Reader
	next []byte
}

func (p *peeker) Read(b []byte) (int, error) {
	if len(p.next) > 0 {
		n := copy(b, p.next)
		p.next = p.next[n:]
		return n, nil
	}
	return p.r.Read(b)
}

func (p *peeker) more() (bool, error) {
	if len(p.next) > 0 {
		return true, nil
	}
	var b [1]byte
	for {
		n, err := p.r.Read(b[:])
		if n > 0 {
			p.next = append(p.next[:0], b[0])
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// resource is an object as the JSON API describes it.
type resource struct {
	Name       string `json:"name"`
	Size       string `json:"size"`
	CRC32C     string `json:"crc32c"`
	Generation string `json:"generation"`
}

func (r *resource) object() (Object, error) {
	if r == nil {
		return Object{}, errors.New("Cloud Storage didn't describe the object")
	}
	size, err1 := strconv.ParseInt(r.Size, 10, 64)
	gen, err2 := strconv.ParseInt(r.Generation, 10, 64)
	crc, err3 := ParseCRC32C(r.CRC32C)
	if err1 != nil || err2 != nil || err3 != nil || size < 0 || gen <= 0 || !ValidObject(r.Name) {
		// None of what it said is repeated: it isn't what it should be, and
		// could be anything, a token included.
		return Object{}, errors.New("Cloud Storage described the object in a way it doesn't")
	}
	return Object{Name: r.Name, Size: size, CRC32C: crc, Generation: gen}, nil
}

// start begins a resumable upload, and returns its session's address.
func (c *Client) start(ctx context.Context, tokens Tokens, bucket, name string) (*url.URL, error) {
	base, err := url.Parse(c.base())
	if err != nil {
		return nil, err
	}
	q := url.Values{"uploadType": {"resumable"}, "name": {name}, "ifGenerationMatch": {"0"}}
	u := c.base() + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?" + q.Encode()
	body, _ := json.Marshal(map[string]string{"name": name, "contentType": "application/zstd"})
	var session *url.URL
	err = c.attempts(ctx, tokens, "starting the upload of "+name, func(tok string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", "application/zstd")
		resp, err := c.http().Do(req)
		if err != nil {
			return &netError{op: "start an upload"}
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusPreconditionFailed {
			return errors.New("the object is there already: this cache's version was saved before")
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return answerError(resp, tok)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Scheme != base.Scheme || loc.Host != base.Host {
			// The session's address is a credential of its own, and
			// must be Cloud Storage's.
			return errors.New("Cloud Storage started the upload without a session at its own address")
		}
		session = loc
		return nil
	})
	return session, err
}

// put sends data, which starts at offset in the upload, and returns the
// object once total, if known, has been sent. A failed request is
// retried from what Cloud Storage says it has. Requests to the session's
// address need no token: the address is the session's credential.
func (c *Client) put(ctx context.Context, session *url.URL, offset int64, data []byte, total int64) (*resource, error) {
	end := offset + int64(len(data))
	sent := offset
	ask := false
	var res *resource
	err := c.attempts(ctx, nil, fmt.Sprintf("sending bytes %d to %d", offset, end), func(string) error {
		for {
			var r *resource
			var have int64
			var err error
			asked := ask
			if ask {
				r, have, err = c.send(ctx, session, 0, nil, total, true)
			} else {
				r, have, err = c.send(ctx, session, sent, data[sent-offset:], total, false)
			}
			if err != nil {
				// What Cloud Storage has is asked before sending again.
				ask = true
				return err
			}
			ask = false
			if r != nil {
				if total < 0 || have != total {
					return errors.New("Cloud Storage finished the upload before it was sent")
				}
				res = r
				return nil
			}
			if have < offset || have > end {
				return fmt.Errorf("Cloud Storage has %d bytes of the upload, outside the %d to %d being sent", have, offset, end)
			}
			if have == end && total < 0 {
				return nil
			}
			if have <= sent && !asked {
				// Nothing more was kept of what was sent: ask, and send
				// again after a wait.
				ask = true
				return errStalled
			}
			sent = have
		}
	})
	return res, err
}

// send sends data from offset, or with status set asks how much Cloud
// Storage has, and returns the object if the upload is finished, or else
// how much it has.
func (c *Client) send(ctx context.Context, session *url.URL, offset int64, data []byte, total int64, status bool) (*resource, int64, error) {
	of := "*"
	if total >= 0 {
		of = strconv.FormatInt(total, 10)
	}
	rng := "bytes */" + of
	if !status && len(data) > 0 {
		rng = fmt.Sprintf("bytes %d-%d/%s", offset, offset+int64(len(data))-1, of)
	}
	if status {
		data = nil
	}
	ctx, cancel := context.WithTimeout(ctx, chunkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, session.String(), bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Range", rng)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, 0, &netError{op: "upload"}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var r resource
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r); err != nil {
			// An answer cut short may be a connection lost: what Cloud
			// Storage has is asked again.
			return nil, 0, &netError{op: "read the end of an upload"}
		}
		size, _ := strconv.ParseInt(r.Size, 10, 64)
		return &r, size, nil
	case http.StatusPermanentRedirect:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		have, err := persisted(resp.Header.Get("Range"))
		return nil, have, err
	case http.StatusNotFound, http.StatusGone:
		return nil, 0, errors.New("the upload's session is gone")
	default:
		return nil, 0, answerError(resp, "")
	}
}

// persisted is how much of an upload Cloud Storage has, by a 308's Range
// header: bytes=0-n, or none at all.
func persisted(h string) (int64, error) {
	if h == "" {
		return 0, nil
	}
	v, ok := strings.CutPrefix(h, "bytes=0-")
	n, err := strconv.ParseInt(v, 10, 64)
	if !ok || err != nil || n < 0 {
		return 0, fmt.Errorf("Cloud Storage's Range %q isn't one it gives", h)
	}
	return n + 1, nil
}

// Open reads object o of bucket, at its generation. Read checks every byte
// against o's size and CRC32C, and fails at the end if they don't match.
// A download that breaks off is resumed where it stopped.
func (c *Client) Open(ctx context.Context, tokens Tokens, bucket string, o Object) io.ReadCloser {
	return &download{c: c, ctx: ctx, tokens: tokens, bucket: bucket, o: o, crc: crc32.New(castagnoli)}
}

type download struct {
	c      *Client
	ctx    context.Context
	tokens Tokens
	bucket string
	o      Object

	body io.ReadCloser
	off  int64
	crc  interface {
		io.Writer
		Sum32() uint32
	}
	// broke counts the reads that broke off, against the retries.
	broke int
	// end is what every read returns once the object is read, or a
	// check of it fails: io.EOF, or why it isn't the object.
	end error
}

func (d *download) Close() error {
	if d.body != nil {
		return d.body.Close()
	}
	return nil
}

func (d *download) open() error {
	q := url.Values{"alt": {"media"}, "generation": {strconv.FormatInt(d.o.Generation, 10)}}
	u := d.c.base() + "/storage/v1/b/" + url.PathEscape(d.bucket) + "/o/" + url.PathEscape(d.o.Name) + "?" + q.Encode()
	return d.c.attempts(d.ctx, d.tokens, "downloading "+d.o.Name, func(tok string) error {
		req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if d.off > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", d.off))
		}
		resp, err := d.c.http().Do(req)
		if err != nil {
			return &netError{op: "download"}
		}
		want := http.StatusOK
		if d.off > 0 {
			want = http.StatusPartialContent
		}
		if resp.StatusCode != want {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return errors.New("the object isn't there at its generation")
			}
			return answerError(resp, tok)
		}
		d.body = resp.Body
		return nil
	})
}

func (d *download) Read(p []byte) (int, error) {
	if d.end != nil {
		return 0, d.end
	}
	for {
		if d.body == nil {
			if err := d.open(); err != nil {
				d.end = err
				return 0, err
			}
		}
		n, err := d.body.Read(p)
		d.off += int64(n)
		d.crc.Write(p[:n])
		if d.off > d.o.Size {
			d.end = fmt.Errorf("downloading %s: it's longer than its %d bytes", d.o.Name, d.o.Size)
			return 0, d.end
		}
		if errors.Is(err, io.EOF) {
			d.end = io.EOF
			if d.off != d.o.Size {
				d.end = fmt.Errorf("downloading %s: it's %d bytes, not %d", d.o.Name, d.off, d.o.Size)
			} else if got := d.crc.Sum32(); got != d.o.CRC32C {
				d.end = fmt.Errorf("downloading %s: its CRC32C is %s, not %s", d.o.Name, FormatCRC32C(got), FormatCRC32C(d.o.CRC32C))
			}
			return n, d.end
		}
		if err != nil {
			// It broke off: start again from here.
			d.body.Close()
			d.body = nil
			d.broke++
			pol := d.c.policy()
			if d.broke >= pol.Attempts {
				d.end = fmt.Errorf("downloading %s: it broke off %d times", d.o.Name, d.broke)
				return n, d.end
			}
			if d.c.Logf != nil {
				d.c.Logf("Downloading %s broke off at byte %d; resuming.", d.o.Name, d.off)
			}
			if err := pol.Pause(d.ctx, pol.Wait(d.broke, 0)); err != nil {
				return n, err
			}
		}
		if n > 0 {
			return n, nil
		}
	}
}

// Refreshing are tokens that expire, and are got again shortly before
// they do, or once one is refused. Once one can't be got, none can.
type Refreshing struct {
	// Get gets a new token, and when it expires.
	Get func(ctx context.Context) (string, time.Time, error)
	// Now is the time; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	tok     string
	expires time.Time
	// err is why a token couldn't be got: every Token after says so.
	err error
}

// RefreshBefore is how long before a token expires it's got again.
const RefreshBefore = time.Minute

// NewRefreshing starts with tok, which expires at expires.
func NewRefreshing(tok string, expires time.Time, get func(ctx context.Context) (string, time.Time, error)) *Refreshing {
	return &Refreshing{Get: get, tok: tok, expires: expires}
}

func (r *Refreshing) Token(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if r.err != nil {
		return "", r.err
	}
	if r.tok != "" && now().Add(RefreshBefore).Before(r.expires) {
		return r.tok, nil
	}
	tok, exp, err := r.Get(ctx)
	if err != nil {
		r.err = err
		return "", err
	}
	r.tok, r.expires = tok, exp
	return tok, nil
}

func (r *Refreshing) Expired(tok string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tok == r.tok {
		r.tok = ""
	}
}

// Check confirms that object o of bucket is there at its generation with
// its size and CRC32C, as Cloud Storage describes it, without reading it.
func (c *Client) Check(ctx context.Context, tokens Tokens, bucket string, o Object) error {
	q := url.Values{"generation": {strconv.FormatInt(o.Generation, 10)}}
	u := c.base() + "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(o.Name) + "?" + q.Encode()
	return c.attempts(ctx, tokens, "checking "+o.Name, func(tok string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.http().Do(req)
		if err != nil {
			return &netError{op: "check an object"}
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return errors.New("the object isn't there at its generation")
		}
		if resp.StatusCode != http.StatusOK {
			return answerError(resp, tok)
		}
		var r resource
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r); err != nil {
			return &netError{op: "read an object's description"}
		}
		got, err := r.object()
		if err != nil {
			return err
		}
		if got.Name != o.Name || got.Size != o.Size || got.CRC32C != o.CRC32C || got.Generation != o.Generation {
			return fmt.Errorf("Cloud Storage has it at generation %d as %d bytes, CRC32C %s, not %d bytes, CRC32C %s",
				got.Generation, got.Size, FormatCRC32C(got.CRC32C), o.Size, FormatCRC32C(o.CRC32C))
		}
		return nil
	})
}
