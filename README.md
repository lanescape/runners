# Lanescape Runners

GitHub Actions runners for the repositories of the `lanescape` organization, and for no one else, on Google Cloud. A job asks for a machine by a label that names only its shape, as it would ask GitHub or any other runner provider, and gets a Compute Engine machine of its own for that job alone, booted from the service's base image, with Docker. A job can run in a container of an image the consumer defines in its own repository and the service builds, and can ask in a step for a work disk, which it can save when it finishes, so that later jobs of the same pull request, or of main, start from it. A job's machine has no cloud identity that can reach anything: jobs that need one sign in with GitHub's token. Nothing runs before a workflow's jobs to set any of this up: the service binds each run itself, at its first job's check, from what GitHub told the job's runner on a fresh machine, confirmed against GitHub's records.

The code is public so that anyone can read it and contribute, but the service itself serves only the `lanescape` organization. Workflows name its machines in `runs-on`, and use it through the actions in `lanescape/actions`, which do nothing of the service's outside the organization.

The specs in [`specs/`](specs/) describe what it does:

- [0001 Architecture](specs/0001-architecture.md): the service, the organization-only guards, endpoints, passes, forks and contributions.
- [0002 Machines](specs/0002-machines.md): selectors, scopes and long-lived branches, binding a run, failing a job with its reason, a machine's life, the work disk, pulling images, preemption, and what's to be confirmed on real GitHub.
- [0003 Base images and saved disks](specs/0003-disks.md): base images, disks' keys, saving and restoring work disks, and deleting them as early as nothing needs them.
- [0004 Code, tests and deployment](specs/0004-deployment.md): Go, our own emulators, CI and CD on Blacksmith, and Terraform through Infrastructure Manager.
- [0005 The interface for actions](specs/0005-interface.md): selectors, `.github/runners.json`, images, work disks and saves, and what the actions in `lanescape/actions` may rely on, versioned as `/v1/`.
- [0006 Consumer images](specs/0006-images.md): the OCI images a consumer defines, their versions, how the service builds, stores and serves them, and when they go.

## Building and testing

The service is being built in stages. So far there's the configuration in `config/`, the emulators in `emulators/`, the pass's lease, the endpoints' authentication, the run binding as a request from the run's first job, which specs 0002 and 0005 now replace, the machines (selectors, making a machine for each queued job, the job-started check, the supervisor's report, cleanup, and the supervisor and job-started hook in `machine/`), saved disks with their restore choices and expiry, long-lived branches' scopes, preemption records, resume images and reruns, the webhook's publishing to Pub/Sub, the interface's constants in `endpoints/`, its emulator in `emulators/service` and the conformance suite in `conformance/` that holds the emulator and the service to the same answers, and the Terraform in `bootstrap/` and `infra/`. Binding a run at its first job's check, failing a job with its reason, consumer images, built as OCI images and pulled into job containers, work disks asked for by the `disk` action, base images with Docker and CD come next; the actions are in `lanescape/actions`.

```sh
go vet ./...
go test ./...
```

Tests need nothing but Go, at the version `go.mod` names: they run against this repository's own emulators of GitHub and Google Cloud.

Licensed under the [MIT License](LICENSE).
