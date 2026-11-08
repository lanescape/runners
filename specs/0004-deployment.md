# Code, tests and deployment

## Code

Go, at the latest stable release, pinned by `go.mod`'s `toolchain` line, using Google's and GitHub's own Go SDKs for their APIs. One module:

- `cmd/runners/`: the service.
- `cmd/proxy/`, `cmd/proxy-git/` and `cmd/forwarder/`: the proxy's HTTP service, its git server and the local forwarder (specs/0006-proxy.md).
- `internal/`: the reconciler, and the clients for GitHub, Compute Engine, Cloud Storage and Pub/Sub, behind interfaces the emulators also serve.
- `emulators/`: the emulators (below).
- `config/`: the organization's ID, shapes, limits and the runner version.
- `machine/`: the supervisor and the job-started hook, embedded into the service's binary, so nothing in a consumer's repository can change what a machine does before its job starts or after it ends.
- `infra/`: the Terraform for everything the service runs on.
- `bootstrap/`: the Terraform that lets `infra/` be applied at all (below).

Never log credentials, JIT runner configurations, OIDC tokens, identity tokens or the App's installation tokens.

## Tests

Tests run against emulators this repository builds and maintains itself, in Go, in `emulators/`. No test depends on an emulator, container or fake from anyone else. Each emulator serves the real API over HTTP, as the SDKs call it, so the service's real clients run against it unchanged, and each keeps state and fails as the real service does:

- **GitHub:** the REST endpoints the service and the actions use, signed webhook deliveries, runner groups and just-in-time registration, environments and their approvals, and an OIDC issuer with its own signing keys and published key set, so tests can mint tokens with each claim wrong in turn. A runner stand-in registers, takes jobs as GitHub would hand them out, including to a machine made for another job, and calls the job-started hook as the real runner does, with a job's environment able to set what a real job can.
- **Compute Engine:** instances, disks, images and zone operations; Spot preemption mid-job; stockouts, quota errors and responses lost after the work was done; `requestId` deduplication; guest attributes; and identity tokens in the full format, signed by the emulator's own keys.
- **Cloud Storage:** objects with generation preconditions, for the lease and the records, and the proxy's temporary and stored objects.
- **Pub/Sub and the scheduler:** signed pushes, redelivery and duplicates.
- **Upstreams for the proxy:** HTTP servers whose responses carry every caching header the proxy honours, that change content, go down and send truncated bodies, and git servers.

Every behaviour in these specs has a test. End-to-end tests run the real service and proxy against the emulators together, with a client that speaks the interface for actions (specs/0005-interface.md) as the actions do: a pull request's run through to its saves, a job handed to the wrong machine, a Spot machine preempted mid-job and its run resumed, a fork's job refused, an approved contribution accepted. Those interface tests are the service's promise to the actions: a change that breaks one breaks `/v1/`. The actions themselves are tested in `lanescape/actions`, against this repository's service and emulators at the version they name. The forwarder is tested natively on Blacksmith's Linux amd64, Linux arm64 and macOS arm64 machines, and CI builds it for every platform runners use. `terraform validate` and `terraform fmt -check` run on every change to `infra/` and `bootstrap/`.

## CI and CD

Workflows here run on Blacksmith, never on this service's own machines, so a broken deploy can always be fixed by the next one. A pull request from a fork runs on GitHub's runners instead; it needs no credentials, since nothing in CI does.

- **`ci.yml`, on pull requests:** `go vet`, `gofmt`, the tests, and the Terraform checks.
- **`cd.yml`, on pushes to main:** the same, then it builds the service's and the proxy's images with `ko`, pushes them to Artifact Registry, and asks Infrastructure Manager to apply `infra/` at that commit, with the images' digests as inputs. One apply changes the infrastructure and deploys both together. A deploy that changes the base images' key leads the service to make new ones (specs/0003-disks.md). The job prints the apply's preview and result, and fails if anything does.

CD signs in with GitHub's token through the workload identity pool `lanescape-runners-github`, whose provider accepts only a push to main from `cd.yml` in this repository. That identity, `lanescape-runners-deployer`, can push to the service's Artifact Registry repository and start applies of the `lanescape-runners` Infrastructure Manager deployment, and nothing else. The apply runs inside Google Cloud as `lanescape-runners-infra`.

That doesn't make CD less powerful than the infra identity: whoever controls the Terraform that CD applies, which is whoever can merge to main, can make the infra identity do anything it may do, provisioners included. So the infra identity's authority is CD's, and it's kept to what `infra/` manages:

- the service's own resources, by their names: its Cloud Run services, the proxy's git machine, topics, subscriptions, scheduler job, buckets, network and its one firewall rule, and secrets' metadata, and the access to each of those, set on the resource itself. That access can still let a merged change give someone more of those resources, such as reading the bucket; it can't reach anything else;
- no project-level IAM administration, no service account creation or key management, no role definitions, and no log sinks. The service's accounts, its custom roles, every project-level grant, and the log sink with its fixed filter for the service's own machines are in `bootstrap/`, which only a maintainer applies, so a merged change can't widen what the service's accounts hold in the project, nor send the project's other logs to the service.

Infrastructure Manager's Terraform runs no provisioners or external programs here: `ci.yml` refuses `provisioner`, `local-exec` and `external` data sources in `infra/`. That keeps a careless change from running code as the infra identity; a deliberate one would be a merged pull request, which its author answers for.

Whoever merges to main deploys. No agent reviews, approves or merges a pull request without the explicit instruction of the person it works for, for that pull request.

## Bootstrap

`bootstrap/` makes what `infra/` can't make for itself: the APIs, the Terraform state bucket, the Artifact Registry repository, every service account (the service's, the machines', the proxy's, the git machine's, the scheduler's, the deployer's and the infra identity), the service's custom roles and every project-level grant, the log sink for Compute Engine's events about the service's machines, and the CD pool. A maintainer applies it by hand, with their own credentials, and again whenever it changes. Its own state is kept in the same state bucket.
