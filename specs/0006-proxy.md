# Proxy

Jobs read upstream sources, such as release archives and git repositories, through the service's proxy, which keeps what it has fetched and serves later reads from it. It's how the service shares across scopes safely: the proxy fetches from upstream itself, and serves only what it fetched, so a job can fill it but never put anything of its own in it. A pull request that reads a new dependency fills the proxy, and main's build after the merge reads it from there.

## Who may use it

Every request is authenticated, and the proxy serves only the organization's consumers:

- **On the service's machines,** with the machine's Compute Engine identity token, in the full format, with the proxy as its audience. The proxy maps the instance to the machine's consumer, as the service does.
- **On other runners,** such as Blacksmith's, with the job's GitHub OIDC token, with the proxy as its audience, whose repository's owner is the organization, and which isn't from a `pull_request_target` run or a fork's pull request, by GitHub's record of the run.

For convenience, the local forwarder, a small Go program that the `proxy` action starts, and that the base image runs on the service's machines, listens on `127.0.0.1`, adds a fresh token to each request, and passes it on, fetching a new token before each expires. Any tool that can be pointed at an HTTP address can then read through the proxy. The forwarder isn't a boundary: a job can mint the same tokens itself, and can hand them to anything while it runs. So the proxy checks, on every request, that the token's machine has an admitted job running now, or that the token's run is still in progress, and refuses once it isn't.

**What a consumer may read** is listed in its default branch's `.github/proxy.json`, read with the App, never the caller's copy:

```json
{
  "http": [
    {"upstream": "https://downloads.example.org/", "immutable": true},
    {"upstream": "https://example.com/files/"}
  ],
  "git": ["https://git.example.org/project.git"]
}
```

A request for anything else is refused, so a job can't use the proxy to fetch whatever it likes at the organization's expense. Each consumer has a limit on what the proxy stores for it, in `config/limits.yaml`, and an object larger than the per-object limit is passed through without being stored.

## HTTP

`/v1/http/<upstream URL>` fetches through a reverse proxy built on Go's `net/http/httputil.ReverseProxy`, using its `Rewrite` hook, which keeps responses in the `lanescape-runners-proxy` bucket.

**One request for everyone.** What the proxy sends upstream never depends on who asked. It builds every outbound request itself: `GET` only, the validated upstream URL and its own `Host`, a fixed `User-Agent`, `Accept-Encoding: identity`, and its own conditional headers when revalidating. Nothing from the caller is passed on: no headers, no cookies, no credentials, no `X-Forwarded-*`, no body. A caller's `HEAD` is answered from the stored response's headers, fetching and storing it with `GET` first if it isn't stored; nothing but a complete `GET` response is ever stored. Other methods, and requests with a body, are refused. So a stored response is one every caller would have got, and the proxy keeps one stored response per URL. A response whose `Vary` names a header the proxy doesn't send varies on nothing, since the proxy always sends the same; `Vary: *` isn't stored.

**What's kept:** only responses with status 200, without `Cache-Control: no-store` or `private`, and no larger than the per-object limit.

**Freshness,** by RFC 9111:

- A response's freshness lifetime is its `s-maxage`, else its `max-age`, else `Expires` minus `Date`; with none of them, it has none. The proxy never guesses a lifetime.
- Its age is computed from `Date`, `Age` and when the proxy received it, as RFC 9111 section 4.2.3 says, so a response that was already old upstream isn't given a new lifetime.
- A response is served without asking upstream only while its age is below its lifetime and it doesn't say `no-cache`. `immutable` means only that, while fresh, it isn't revalidated; it doesn't extend the lifetime.
- Otherwise the proxy revalidates first, with `If-None-Match` and the stored `ETag`, or `If-Modified-Since` and the stored `Last-Modified`. On `304 Not Modified` it serves what it has and updates the stored headers; on `200` it replaces it. A response with neither validator is fetched again in full.
- If upstream can't be reached, times out, or answers `500`, `502`, `503` or `504` while revalidating, a stale response is served only within its `stale-if-error` seconds, and never one marked `no-cache`, `must-revalidate` or `proxy-revalidate`, or carrying `s-maxage`, which binds a shared cache to revalidate. Any other answer, such as `403` or `404`, is passed on, and the stored response is dropped. Otherwise the request fails, as it would have without the proxy.
- A caller's own `Cache-Control` is ignored: it can't make the proxy skip revalidation or fetch again. Every response the proxy serves carries its current `Age`.

**A consumer's `immutable` upstream** is a separate policy, with its own precedence: `no-store` still means nothing is stored, and otherwise a stored response from that upstream is served without revalidation, whatever its own headers say. It's for upstreams whose URLs never change content, such as versioned release archives. It's the consumer's word: an entry that isn't true serves old content.

**Callers' own conditions and ranges** are answered from the stored response, never passed upstream, by RFC 9110's precedence. `If-None-Match` is evaluated with weak comparison, and when it's present `If-Modified-Since` is ignored; either may give `304`. Then `If-Range`, which matches only a strong `ETag`, or a `Last-Modified` date that's exactly equal and strong by RFC 9110 section 8.8.2.2, at least a second older than the response's `Date`, and `Range`, which give `206` for a satisfiable range of the stored response, the whole response with `200` when `If-Range` doesn't match, and `416` for a range outside it. A range of something not yet stored makes the proxy fetch and store the whole response first, then answer.

**Complete or not at all:** a response is streamed to the caller and the bucket at once, into a temporary object, which becomes the stored one only once the whole body has arrived, matching its `Content-Length` and, if upstream sent one, its `Content-Digest` over the bytes received. An interrupted or mismatched download is never kept, and the proxy breaks the connection to the caller instead of ending the response cleanly. A caller may still have read every byte by then: the proxy protects what it stores, not what a caller has already read. `fetch` writes to a temporary file and moves it into place only once its expected SHA-256, when given, matches; a consumer that wants end-to-end integrity gives one. Concurrent requests for the same URL that isn't stored wait for one fetch, and are served from what it stored. If that fetch turns out not to be storable, by its headers or its size, the waiting requests are let go to fetch for themselves, each passed through and none kept; for those, and for any response too large to store, a caller's `Range` and `If-Range` are the only caller headers passed upstream, together, since nothing of the answer is shared.

## Git

`/v1/git/<repository URL>` serves git's smart HTTP protocol, for reading only, from a mirror of each allowlisted repository, on the proxy's git machine, `lsr-proxy-git`: a standard machine with a persistent disk, in the service's network, reachable only from the service's machines, at its internal address (specs/0002-machines.md).

- It serves with `git http-backend` through Go's `net/http/cgi`, with `http.receivepack` set to `false`, no `REMOTE_USER`, and dumb HTTP off. Before anything reaches the CGI, the server refuses every request but `GET .../info/refs?service=git-upload-pack` and `POST .../git-upload-pack`, so nothing can be pushed.
- Each allowlisted URL maps to a mirror directory the server owns, named by a hash of the URL; a request names a URL, never a path.
- When a client asks for a mirror's refs, the proxy first fetches from upstream if it last did more than 5 minutes ago.
- When a client wants an object the mirror lacks, such as a commit asked for by hash, the proxy fetches it from upstream, then answers.
- Only the proxy's own fetches from upstream change a mirror, so its refs are upstream's as of the last fetch, and its objects, named by their hashes, are upstream's.
- A mirror unused for 90 days is deleted.

Jobs on other runners read git straight from upstream.

## Running it

The HTTP proxy is a Cloud Run service, `lanescape-runners-proxy`, with its own account, which can read and write only its own bucket and read what it needs to check callers. The git machine has its own account too, which can do no more than check callers. Both are defined in `infra/`, and both are versioned as the service is (specs/0001-architecture.md). The proxy keeps no credentials for any upstream, and never sends any.
