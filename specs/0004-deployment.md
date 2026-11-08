# Code, tests and deployment

## Code

Go, at the latest stable release, pinned by `go.mod`'s `toolchain` line. One module:

- `cmd/runners/`: the service.
- `internal/`: the reconciler, and GitHub and Compute Engine clients behind interfaces small enough to fake.
- `config/`: consumers, shapes and the runner version (specs/0001-architecture.md, specs/0002-machines.md).
- `machine/`: the startup script, the job-started hook and the builder script, embedded into the service's binary, so nothing in a consumer's repository can change what a machine does before its job starts.
- `infra/`: the Terraform for everything the service runs on.
- `bootstrap/`: the Terraform that lets `infra/` be applied at all (below).

Never log credentials, JIT runner configurations, OIDC tokens or the App's installation tokens.

## Tests

Every reconcile behaviour in these specs has a test, against fakes of GitHub and Compute Engine that keep state and fail as the real ones do: quota and stockout errors, preemption mid-job, lost and duplicated events, a runner that never registers, a job handed to a different machine for the same labels, and a pass that dies part way. The images API is tested with tokens signed by a test key, including each claim being wrong in turn. `terraform validate` and `terraform fmt -check` run on every change to `infra/` and `bootstrap/`.

## CI and CD

Workflows here run on Blacksmith, never on this service's own machines, so a broken deploy can always be fixed by the next one. A pull request from a fork runs on GitHub's runners instead, by the expression in specs/0001-architecture.md; it needs no credentials, since nothing in CI does.

- **`ci.yml`, on pull requests:** `go vet`, `gofmt`, the tests, and the Terraform checks.
- **`cd.yml`, on pushes to main:** the same, then it builds the service's image with `ko`, pushes it to Artifact Registry, and asks Infrastructure Manager to apply `infra/` at that commit, with the image's digest as an input. One apply changes the infrastructure and deploys the service together. The job prints the apply's preview and result, and fails if the apply does.

CD signs in with GitHub's token through the workload identity pool `lanescape-runners-github`, whose provider accepts only a push to main from `cd.yml` in this repository. That identity, `lanescape-runners-deployer`, can push to the service's Artifact Registry repository and start applies of the `lanescape-runners` Infrastructure Manager deployment, and nothing else. The apply runs inside Google Cloud as `lanescape-runners-infra`.

That doesn't make CD less powerful than the infra identity: whoever controls the Terraform that CD applies, which is whoever can merge to main, can make the infra identity do anything it may do, provisioners included. So the infra identity's authority is CD's, and it's kept to what `infra/` manages:

- the service's own resources, by their names: its Cloud Run service, topics, subscriptions, scheduler job, bucket, network and secrets' metadata, and the access to each of those, set on the resource itself. That access can still let a merged change give someone more of those resources, such as reading the bucket; it can't reach anything else;
- no project-level IAM administration, no service account creation or key management, no role definitions, and no log sinks. The service's accounts, its custom roles, every project-level grant, and the log sink with its fixed filter for the service's own machines are in `bootstrap/`, which only a maintainer applies, so a merged change can't widen what the service's accounts hold in the project, nor send the project's other logs to the service.

Infrastructure Manager's Terraform runs no provisioners or external programs here: `ci.yml` refuses `provisioner`, `local-exec` and `external` data sources in `infra/`. That keeps a careless change from running code as the infra identity; a deliberate one would be a merged pull request, which its author answers for.

Whoever merges to main deploys. No agent reviews, approves or merges a pull request without the explicit instruction of the person it works for, for that pull request.

## Bootstrap

`bootstrap/` makes what `infra/` can't make for itself: the APIs, the Terraform state bucket, the Artifact Registry repository, every service account (the service's, the machines', the scheduler's, the deployer's and the infra identity), the service's custom roles and every project-level grant, the log sink for Compute Engine's events about the service's machines, and the CD pool. A maintainer applies it by hand, with their own credentials, and again whenever it changes. Its own state is kept in the same state bucket.
