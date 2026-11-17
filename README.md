# Lanescape Runners

GitHub Actions runners for the repositories of the `lanescape` organization, and for no one else, on Google Cloud. A job asks for a machine by a label that names only its shape, as it would ask GitHub or any other runner provider, and gets a Compute Engine machine of its own for that job alone, booted from the service's Debian 13 base image. The job sets the machine up in its own steps, from its own commit, with passwordless sudo, and can ask in a step for a work disk, onto which it restores caches and from which it saves them, as archives in the service's caches' bucket, so that later jobs of the same pull request, or of main, start from them. A job's machine has no cloud identity that can reach anything: jobs that need one sign in with GitHub's token. Nothing runs before a workflow's jobs to set any of this up: the service binds each run itself, at its first job's check, from what GitHub told the job's runner on a fresh machine, confirmed against GitHub's records.

The code is public so that anyone can read it and contribute, but the service itself serves only the `lanescape` organization. Workflows name its machines in `runs-on`, and use it through the actions in `actions/`, `lanescape/runners/actions/<name>@v1`, which do nothing of the service's outside the organization.

The specs in [`specs/`](specs/) describe what it does:

- [0001 Architecture](specs/0001-architecture.md): the service, the organization-only guards, endpoints, passes, forks and contributions.
- [0002 Machines](specs/0002-machines.md): selectors, scopes and long-lived branches, binding a run, failing a job with its reason, a machine's life, the work disk, setting up a machine, preemption, and what's to be confirmed on real GitHub.
- [0003 Base images, caches and resume images](specs/0003-disks.md): base images, caches' keys, restoring and saving caches, resume images, and deleting them as early as nothing needs them.
- [0004 Code, tests and deployment](specs/0004-deployment.md): Go, our own emulators, CI and CD on Blacksmith, Terraform through Infrastructure Manager, and the actions' `v1` tag, which moves once its commit is deployed.
- [0005 The interface for actions](specs/0005-interface.md): selectors, `.github/runners.json`, work disks and caches, and what the actions may rely on, versioned as `/v1/`.
- [0006 Build tools](specs/0006-tools.md): the organization's private bucket of build tools, its layout, who reads it and how; apart from the service.
- [0007 Actions](specs/0007-actions.md): the actions in `actions/`, `disk`, `checkout`, `cache` and `save-cache`, how each degrades outside the organization, and how they're built and tested.

## Build tools

The organization's builds can copy platform-specific tools, SDKs and other files onto their runners from a private bucket, `gs://lanescape-ci-tools`, laid out as `<platform>/<tool>/<version>/<file>`, each file with its `<file>.sha256` beside it, such as `macos/sdk/26.2/MacOSX26.2.sdk.tar.zst`. Objects never change: a new version gets a new path. A job of any of the organization's repositories reads it by signing in with GitHub's token through the provider `projects/168731703350/locations/global/workloadIdentityPools/lanescape-ci-tools/providers/github` as `lanescape-ci-tools-reader@lanescape-dev.iam.gserviceaccount.com`; a fork's job and a `pull_request_target` run's can't. Only the maintainer uploads. It's no part of the service, which never reads it, and its Terraform, in `tools/`, is applied by hand by the maintainer, with its own state (specs/0006-tools.md, `tools/README.md`).

## Building and testing

The service is being built in stages. So far there's the configuration in `config/`, the emulators in `emulators/`, the pass's lease, the endpoints' authentication, the machines (selectors that name only a shape, making a machine for each queued job, binding a run at its first job's check, failing a job with its reason, the supervisor's report, cleanup and boot logs from Cloud Logging), the service's programs for the base images in `machine/` (the supervisor, the job-started hook's script and program, and the program, unit and rule that mount a work disk at `/work`), work disks asked for by a job's disk requests, caches restored and saved through the cache requests, with their restore choices, downscoped tokens and expiry, long-lived branches' scopes, preemption records, resume images and reruns, the consumer's file and its caches' keys in `internal/consumer`, the webhook's publishing to Pub/Sub, the interface's constants in `endpoints/`, its emulator in `emulators/service` and the conformance suite in `conformance/` that holds the emulator and the service to the same answers, the Debian 13 base images, which the service makes itself on builder machines when a deploy changes their key, from `config/base.yaml`, the builder's script `machine/build-base.sh` and the programs in `machine/`, the Terraform in `bootstrap/` and `infra/`, the build tools' bucket in `tools/`, and CD, `cd.yml`, which builds the service with `ko` and applies `infra/` through Infrastructure Manager once the maintainer has set its variables (`bootstrap/README.md`). The actions, `disk`, `checkout`, `cache` and `save-cache`, are in `actions/`, with what they share in `actions/internal/`.

```sh
go vet ./... && go vet -tags e2e ./...
gofmt -l .
go test -race ./...         # with the actions' end-to-end tests against the service emulator
go test -short -race ./...  # without them
```

Tests need nothing but Go, at the version `go.mod` names: they run against this repository's own emulators of GitHub and Google Cloud.

Licensed under the [MIT License](LICENSE).
