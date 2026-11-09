# The interface for actions

Consumers' workflows use the service through actions maintained in their own repository, `lanescape/actions`, which call this service the way many actions call an API maintained elsewhere. This spec is everything those actions may rely on. It's versioned as `/v1/`, and stays compatible: a change that would break an action comes as `/v2/`, beside `/v1/`, which goes only once no action uses it (specs/0001-architecture.md).

## Where the service is

The service's address and the proxy's are constants of this module's package `github.com/lanescape/runners/endpoints`, `endpoints.Service` and `endpoints.Proxy`, at the version an action's `go.mod` names, beside the organization's immutable ID, `endpoints.Organization`, which is what an action compares `github.repository_owner_id` with, never the organization's name. Each is also the audience of every token sent to it. Every request carries its token as `Authorization: Bearer <token>`.

## Binding a run

`POST /v1/bind` at `endpoints.Service`, with the job's GitHub OIDC token, audience `endpoints.Service`, binds the run's scope (specs/0002-machines.md), or returns the binding the run already has. A run's first job calls it.

- **The body** is empty, except from a consumer's bootstrap workflow in a `pull_request_target` run, where it's what the job's own event says: `{"pull_request": <number>, "head_repository_id": <id>, "head_sha": "<commit>"}`.
- **The answer** is `200` with `{"scope": "main" | "pr<number>" | "read"}`; `403` with `{"reason": "<why>"}` when the token or its run isn't one the service binds, or the body isn't a binding request; or `503` with `{"retryable": true}` and a `Retry-After`, when GitHub couldn't confirm an answer, which the caller retries with backoff. Answers may carry other fields, which callers ignore; nothing else is answered.

## Selectors

A job's selector is `lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot][-disk-<family>-<key>]-<scope>`, in lowercase, with the scope the binding returned (specs/0002-machines.md). `<family>` is 1 to 32 lowercase letters, digits and hyphens, starting with a letter; `<key>` is exactly 20 lowercase hex digits, such as the first 20 of a SHA-256. The shapes the service offers are in `config/shapes.yaml`.

## A consumer's files

- **`.github/runners.json`:** an object from each runner name the consumer uses, a shape such as `lanescape-80vcpu-linux-amd64`, to its fallback outside the organization, a label or a list of labels; and, if the consumer runs approved contributions, `"bootstrap"`, the `job_workflow_ref` of its bootstrap workflow, `<owner>/<repo>/.github/workflows/<file>@refs/heads/<default branch>`. Two readers read it from different commits, for different things. The `runners` action reads the names and fallbacks from the run's own commit, so a pull request that adds a runner can use it; the service decides nothing from those, since it checks every selector itself. The service reads only `"bootstrap"`, and only from the default branch, never a caller's copy.
- **`.github/proxy.json`:** what the consumer may read through the proxy, in the format specs/0006-proxy.md gives, which is part of this interface: `http`, a list of `{"upstream": "<URL prefix>", "immutable": <bool>}`, and `git`, a list of repository URLs. The service and the proxy read it only from the default branch, and so must an action that wants to know which reads to send through the proxy; reading it grants nothing.

## Asking to save a work disk

On the service's machines, a job asks to save its work disk by writing `{"save": true}` to `/var/lib/lanescape-runners/save`, in a directory the runner's user may write, which the supervisor reads once the runner has exited (specs/0002-machines.md). The file is absent on other runners. Anything else in it, a link in its place, or no file at all, asks for no save. Whether the disk is saved is still the service's to decide, from GitHub's conclusion for the job and its run's scope (specs/0003-disks.md); the job learns nothing of it, since it has ended.

## The machines

Jobs on the service's machines run as the runner's user, with passwordless sudo, as on GitHub's runners, and find their work disk at `/work` (specs/0002-machines.md).

## Reading through the proxy

The proxy's paths are `/v1/http/<upstream URL>` and `/v1/git/<repository URL>` (specs/0006-proxy.md). Jobs reach them through the local forwarder, this repository's command `github.com/lanescape/runners/cmd/forwarder`, which knows the proxy's addresses for its version and signs every request itself.

- **On the service's machines,** the base image runs the forwarder for the machine, signing as the machine. Its address is the one line `http://127.0.0.1:<port>` in `/var/lib/lanescape-runners/proxy`, written only once it's listening.
- **What each runner can reach:** HTTP through the proxy, on the service's machines and on other runners; git through the proxy, only on the service's machines, since the git mirror is reachable only from them. On other runners, git reads go straight to upstream.
- **Elsewhere,** an action builds the forwarder from this module at the version its own `go.mod` names, and starts it as `forwarder -listen 127.0.0.1:0 -address-file <file>`. It signs with the job's GitHub OIDC token, which it gets from the job's `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, with the proxy as audience, so the job needs `id-token: write`. It writes the same one line to `<file>` only once it's listening, and exits with an error, writing nothing, if it can't start or the proxy refuses its token. It stops when the job does.
- **A request** goes to the forwarder's address with the proxy's own path, the upstream URL appended as it is, with its scheme and query, never encoded again: `http://127.0.0.1:<port>/v1/http/https://example.org/a.tar.gz`, or for git, `http://127.0.0.1:<port>/v1/git/https://git.example.org/project.git`, to which git adds its own `/info/refs` and `/git-upload-pack`. The forwarder sends it on to the proxy's matching address unchanged, apart from its own credentials.
