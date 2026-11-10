# Code, tests and deployment

## Code

Go, at the latest stable release, pinned by `go.mod`'s `toolchain` line, using Google's and GitHub's own Go SDKs for their APIs. One module:

- `cmd/runners/`: the service.
- `internal/`: the reconciler, and the clients for GitHub, Compute Engine, Cloud Storage and Pub/Sub, behind interfaces the emulators also serve.
- `emulators/`: the emulators (below), and `emulators/service`, the emulator of the interface for actions that consumers test against (specs/0005-interface.md).
- `conformance/`: the interface's test suite, which holds the emulator and the service to the same answers.
- `config/`: the organization's ID, shapes and the runner version.
- `machine/`: the supervisor, which also runs image builds, and the job-started hook, embedded into the service's binary and baked into the base images, so that what a machine does before its job starts and after it ends is the service's, unless the consumer's own image changes it for its own scope's machines (specs/0006-images.md).
- `infra/`: the Terraform for everything the service runs on.
- `bootstrap/`: the Terraform that lets `infra/` be applied at all (below).

Never log credentials, JIT runner configurations, OIDC tokens, identity tokens or the App's installation tokens.

## Tests

Tests run against emulators this repository builds and maintains itself, in Go, in `emulators/`. No test depends on an emulator, container or fake from anyone else. Each emulator serves the real API over HTTP, as the SDKs call it, so the service's real clients run against it unchanged, and each keeps state and fails as the real service does:

- **GitHub:** the REST endpoints the service and the actions use, signed webhook deliveries, pull requests that close, merge and reopen, runner groups and just-in-time registration, environments and their approvals, and an OIDC issuer with its own signing keys and published key set, so tests can mint tokens with each claim wrong in turn. A runner stand-in registers, takes jobs as GitHub would hand them out, including to a machine made for another job, and calls the job-started hook as the real runner does, with a job's environment able to set what a real job can; after a hook that fails, it still runs the steps whose condition is `always()` or `failure()`, as the real runner does.
- **Compute Engine:** instances, disks, images and zone operations; serial console output; Spot preemption mid-job; stockouts, quota errors and responses lost after the work was done; `requestId` deduplication; guest attributes; and identity tokens in the full format, signed by the emulator's own keys.
- **Cloud Storage:** objects with generation preconditions, for the lease and the records.
- **Pub/Sub and the scheduler:** publishing, signed pushes, redelivery and duplicates.

Every behaviour in these specs has a test. End-to-end tests run the real service against the emulators, with a client that speaks the interface for actions (specs/0005-interface.md) as the actions do: a pull request's run bound from GitHub's records and through to its saves, a job failed with its reason, a job handed to the wrong machine, a Spot machine preempted mid-job and its run resumed, a fork's job refused, an approved contribution bound by its contribution job, and an image built, booted, failed and expired. Those interface tests are the service's promise to the actions: a change that breaks one breaks `/v1/`. The conformance suite in `conformance/` runs the same cases against the real service, wired to the emulators, and against `emulators/service`, with its own model of GitHub and on a GitHub emulator, and each must pass every one (specs/0005-interface.md, Testing against the emulator). The actions themselves are tested in `lanescape/actions`, against `emulators/service` at the version of this module they pin, never the real service; conformance is this repository's promise that what passes there passes here. `terraform validate` and `terraform fmt -check` run on every change to `infra/` and `bootstrap/`.

## CI and CD

Workflows here run on Blacksmith, never on this service's own machines, so a broken deploy can always be fixed by the next one. A pull request from a fork runs on GitHub's runners instead; it needs no credentials, since nothing in CI does.

- **`ci.yml`, on pull requests:** `go vet`, `gofmt`, the tests, and the Terraform checks.
- **`cd.yml`, on pushes to main:** the same, then it builds the service's image with `ko`, pushes it to Artifact Registry, and asks Infrastructure Manager to apply `infra/` from this repository's git source at that commit, never an upload, which would need project-wide Cloud Storage access, with the image's digest as an input and the artifacts bucket `lanescape-runners-infra-manager` for its artifacts and logs. One apply changes the infrastructure and deploys the service together. A deploy that changes the base images' key leads the service to make new ones (specs/0003-disks.md). The job prints the apply's preview and result, and fails if anything does.

`infra/` gives the service its settings in its environment: `RUNNERS_AUDIENCE`, its own Cloud Run address, which must be `endpoints.Service` (specs/0005-interface.md) or it refuses to start; `RUNNERS_PROJECT`, the project its machines are in; `RUNNERS_BUCKET`; `RUNNERS_WEBHOOK_TOPIC`, the full name of the topic the webhook publishes to, which `bootstrap/` makes; `RUNNERS_GITHUB_APP_ID`; the files of the App's key and webhook secret, mounted from Secret Manager; and `RUNNERS_RECONCILE_ACCOUNTS`, the scheduler's and the push subscriptions' accounts. Its Cloud Run service keeps CPU allocated outside requests, since a supervisor's report starts a pass after its response has been sent.

CD signs in with GitHub's token through the workload identity pool `lanescape-runners-github`, whose provider accepts only a push to main from `cd.yml` in this repository. That identity, `lanescape-runners-deployer`, can push to the service's Artifact Registry repository and start applies of the `lanescape-runners` Infrastructure Manager deployment, and nothing else. The apply runs inside Google Cloud as `lanescape-runners-infra`.

That doesn't make CD less powerful than the infra identity: whoever controls the Terraform that CD applies, which is whoever can merge to main, can make the infra identity do anything it may do, provisioners included. So the infra identity's authority is CD's, and it's kept to what `infra/` manages:

- the service's own resources, by their names: its Cloud Run service, which it deploys but can't make or delete, its bucket, and secrets' metadata, and the access to each of those, set on the resource itself. That access can still let a merged change give someone more of those resources, such as reading the bucket; it can't reach anything else;
- no project-level IAM administration, no service account creation or key management, no role definitions, and no log sinks. The service's accounts, its custom roles, every project-level grant, and the log sink with its fixed filter for the service's own machines are in `bootstrap/`, which only a maintainer applies, so a merged change can't widen what the service's accounts hold in the project, nor send the project's other logs to the service;
- nothing IAM can't limit by name. The network and its subnetwork, the Pub/Sub topics and push subscriptions, the scheduler job, and the Cloud Run service's first, empty version are in `bootstrap/` too, since an identity that could make or change them could change every other team's networks, topics, subscriptions, jobs and services in the project. The Cloud Run service's first version is an unreachable shell, which `infra/` adopts and deploys.

Infrastructure Manager's Terraform runs no provisioners or external programs here: `ci.yml` refuses `provisioner`, `local-exec` and `external` data sources in `infra/`. That keeps a careless change from running code as the infra identity; a deliberate one would be a merged pull request, which its author answers for.

Whoever merges to main deploys. No agent reviews, approves or merges a pull request without the explicit instruction of the person it works for, for that pull request.

## Bootstrap

`bootstrap/` makes what `infra/` can't make for itself, or mustn't be able to change: the APIs, the Terraform state bucket, Infrastructure Manager's artifacts bucket, `lanescape-runners-infra-manager`, the Artifact Registry repository, every service account (the service's, the machines', the scheduler's, the push subscriptions', `lanescape-runners-push`, the deployer's and the infra identity), the service's custom roles and every project-level grant, the network with its subnetwork and no firewall rules, the Pub/Sub topics and push subscriptions, the scheduler job, the Cloud Run service's first, empty version, the log sink for Compute Engine's events about the service's machines, a log view of the project's default log bucket that holds only the serial console entries of the service's machines, which the service's account alone may read, for their boot logs (specs/0002-machines.md), and the CD pool. A maintainer applies it by hand, with their own credentials, and again whenever it changes. Its own state is kept in the same state bucket.

## The GitHub App

The maintainer makes and configures the "Lanescape Runners" App by hand, private to the organization (specs/0001-architecture.md). Besides what it needs for runner groups, runners, jobs, runs and environments' approvals, its permissions include **Contents: read**, to read `.github/runners.json`, the files that disks' keys depend on and images' directories at a run's commit, a contributor's pull request's head commit included; **Pull requests: read**, to read a pull request's state, base and head, and it subscribes to the **Pull request** event, whose closing deletes the pull request's scope at once (specs/0003-disks.md), as well as **Workflow job** and **Workflow run**. The webhook ignores every other event, and every other action of a pull request's.
