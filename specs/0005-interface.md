# The interface for actions

Consumers' workflows use the service through actions maintained in their own repository, `lanescape/actions`, which call this service the way many actions call an API maintained elsewhere. This spec is everything those actions may rely on. It's versioned as `/v1/`, and stays compatible: a change that would break an action comes as `/v2/`, beside `/v1/`, which goes only once no action uses it (specs/0001-architecture.md).

## Where the service is

The service's address is a constant of this module's package `github.com/lanescape/runners/endpoints`, `endpoints.Service`, at the version an action's `go.mod` names, beside the organization's immutable ID, `endpoints.Organization`, which is what an action compares `github.repository_owner_id` with, never the organization's name. The address is also the audience of every token sent to it. Every request to the service carries its token as `Authorization: Bearer <token>`.

## Binding a run

`POST /v1/bind` at `endpoints.Service`, with the job's GitHub OIDC token, audience `endpoints.Service`, binds the run's scope (specs/0002-machines.md), or returns the binding the run already has. A run's first job calls it.

- **The body** is empty, except from a consumer's bootstrap workflow in a `pull_request_target` run, where it's what the job's own event says: `{"pull_request": <number>, "head_repository_id": <id>, "head_sha": "<commit>"}`.
- **The answer** is `200` with `{"scope": "main" | "branch-<name>" | "pr<number>" | "read"}`; `403` with `{"reason": "<why>"}` when the token or its run isn't one the service binds, or the body isn't a binding request; or `503` with `{"retryable": true}` and a `Retry-After`, when GitHub couldn't confirm an answer, or its signing keys couldn't be read, which the caller retries with backoff. Answers may carry other fields, which callers ignore; nothing else is answered.

## Selectors

A job's selector is `lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot][-disk-<family>-<key>]-<scope>`, in lowercase, with the scope the binding returned (specs/0002-machines.md). `<family>` is 1 to 32 lowercase letters, digits and hyphens, starting with a letter; `<key>` is exactly 20 lowercase hex digits, such as the first 20 of a SHA-256. The shapes the service offers are in `config/shapes.yaml`. A cache entry's family and key follow the same rules.

## A consumer's files

- **`.github/runners.json`:** an object from each runner name the consumer uses, a shape such as `lanescape-80vcpu-linux-amd64`, to its fallback outside the organization, a label or a list of labels; if the consumer runs approved contributions, `"bootstrap"`, the `job_workflow_ref` of its bootstrap workflow, `<owner>/<repo>/.github/workflows/<file>@refs/heads/<default branch>`; and if it has long-lived branches, such as release branches, `"branches"`, an object from each branch's name to the name of its scope, 1 to 16 lowercase letters and digits, starting with a letter: `{"support/v1": "v1"}` gives pushes to `support/v1` the scope `branch-v1` (specs/0002-machines.md, Scope). Two readers read the file from different commits, for different things. The `runners` action reads the names and fallbacks from the run's own commit, so a pull request that adds a runner can use it; the service decides nothing from those, since it checks every selector itself. The service reads only `"bootstrap"` and `"branches"`, and only from the default branch, never a caller's copy.

## Asking to save a work disk

On the service's machines, a job asks to save its work disk, as the `save-disk` action does, by writing `{"save": true}` to `/var/lib/lanescape-runners/save`, in a directory the runner's user may write, which the supervisor reads once the runner has exited (specs/0002-machines.md). The file is absent on other runners. Anything else in it, a link in its place, or no file at all, asks for no save. Whether the disk is saved is still the service's to decide, from GitHub's conclusion for the job and its run's scope (specs/0003-disks.md); the job learns nothing of it, since it has ended.

## The machines

Jobs on the service's machines run as the runner's user, with passwordless sudo, as on GitHub's runners, and find their work disk at `/work` (specs/0002-machines.md).

## The cache

Jobs on any runner save and restore cache entries through three requests to `endpoints.Service`, each with the job's GitHub OIDC token, audience `endpoints.Service`, so the job needs `id-token: write`; the token's job must be running, and its run bound (specs/0006-cache.md, Who may use it). Each body is a JSON object, and one with other fields is refused. Each answer is `200` with the object below; `403` with `{"reason": "<why>"}` when the service won't do it, such as for a run that isn't bound, a save in the `read` scope, or a commit that doesn't match its upload, which the caller doesn't retry; or `503` with `{"retryable": true}` and a `Retry-After`, when GitHub or the bucket couldn't confirm an answer, which the caller retries with backoff. Answers may carry other fields, which callers ignore.

`<platform>` is `<os>-<arch>`, the runner's `RUNNER_OS` and `RUNNER_ARCH` in lowercase, joined by a hyphen, such as `linux-x64`, `linux-arm64`, `macos-arm64` or `windows-x64`: each part 1 to 16 lowercase letters and digits.

- **`POST /v1/cache/restore`** with `{"family": "<family>", "key": "<key>", "platform": "<platform>"}`, and optionally `"lookup": true`, answers `{"entry": null}` when there's nothing to restore, or `{"entry": {"scope": "<scope>", "key": "<key>", "step": "<step>", "size": <bytes>, "sha256": "<64 hex digits>", "url": "<URL>", "expires": "<RFC 3339 time>"}}`. A lookup's answer has no `url` or `expires`: it reports the run's choice, which it records as a restore does, without handing out the entry or marking it used. `scope` and `key` are the entry's, and `step` is the step of the restore order it came from: `pull_request_exact`, `pull_request_newest`, `main_exact`, `main_newest`, `branch_exact` or `branch_newest`. The same request from any job of the same run answers the same entry, with a new URL. The URL is read with `GET`, with or without `Range`, and no `Authorization` header, until it expires; a caller that needs longer asks again. The caller checks the size and SHA-256 over every byte it read before it uses any of them, and treats a mismatch as nothing restored.
- **`POST /v1/cache/save`** with `{"family": "<family>", "key": "<key>", "platform": "<platform>"}` reserves an upload, and answers `{"id": "<reservation ID>", "url": "<URL>", "headers": {"<name>": "<value>", ...}, "expires": "<RFC 3339 time>"}`. The caller starts a resumable upload by sending `POST` to the URL, with exactly the headers given and an empty body; Cloud Storage answers with the upload's session URI in `Location`, to which the caller sends the bytes with `PUT`, in chunks with `Content-Range`, and resumes after a failure, as Cloud Storage's resumable uploads take them. A run whose scope saves nothing gets `403`.
- **`POST /v1/cache/commit`** with `{"id": "<reservation ID>", "size": <bytes>, "sha256": "<64 hex digits>"}`, from the job that reserved it, before the reservation expires, answers `{"state": "pending"}`: the entry becomes restorable once GitHub shows the job succeeded, and goes otherwise.

A URL, an upload's session URI and a token are each a credential: the caller never logs one, nor puts one in an output or a file other jobs can read.

**Contributions.** An approved contribution's job that uses the cache, normally one on a runner outside the service, needs `id-token: write`, so the contributor's code can mint the consumer's tokens while it runs. With the service such a token can do only what its job may (specs/0006-cache.md, Who may use it): `/v1/bind` gives it the binding its bootstrap workflow already made, never another; it acts only on its own run and job; and the cache gives it only its run's restore chain and its own scope's saves. **Every other party that trusts the consumer's OIDC tokens must refuse one whose `event_name` is `pull_request_target`.**
