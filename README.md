# Lanescape Runners

GitHub Actions runners for the repositories of the `lanescape` organization, and for no one else, on Google Cloud. A job asks for a machine by label, as it would ask GitHub or any other runner provider, and gets a Compute Engine machine of its own for that job alone. A job can save its work disk when it finishes, and later jobs of the same pull request, or of main, start from it. Jobs on any runner, the service's or another's, can save and restore build state through the service's cache, scoped the same way. A job's machine has no cloud identity that can reach anything: jobs that need one sign in with GitHub's token.

The code is public so that anyone can read it and contribute, but the service itself serves only the `lanescape` organization. Workflows use it through the actions in `lanescape/actions`, which work anywhere: outside the organization they fall back to GitHub's runners, or to a fork's own, and restore and save nothing.

The specs in [`specs/`](specs/) describe what it does:

- [0001 Architecture](specs/0001-architecture.md): the service, the organization-only guards, endpoints, passes, forks and contributions.
- [0002 Machines](specs/0002-machines.md): selectors, scopes and long-lived branches, a machine's life, the work disk and preemption.
- [0003 Images and saved disks](specs/0003-disks.md): base images, saving and restoring work disks, and deleting them as early as nothing needs them.
- [0004 Code, tests and deployment](specs/0004-deployment.md): Go, our own emulators, CI and CD on Blacksmith, and Terraform through Infrastructure Manager.
- [0005 The interface for actions](specs/0005-interface.md): what the actions in `lanescape/actions` may rely on, versioned as `/v1/`.
- [0006 Cache](specs/0006-cache.md): saving and restoring build state from any runner, through signed URLs, in the same scopes as saved disks.

## Building and testing

The service is being built in stages. So far there's the configuration in `config/`, the emulators in `emulators/`, the pass's lease, the endpoints' authentication, the run binding, the machines (selectors, making a machine for each queued job, the job-started check, the supervisor's report, cleanup, and the supervisor and job-started hook in `machine/`), saved disks with their restore choices and expiry, long-lived branches' scopes, preemption records, resume images and reruns, the webhook's publishing to Pub/Sub, the cache, the interface's constants in `endpoints/`, its emulator in `emulators/service` and the conformance suite in `conformance/` that holds the emulator and the service to the same answers, and the Terraform in `bootstrap/` and `infra/`. Base images and CD come next; the actions are in `lanescape/actions`.

```sh
go vet ./...
go test ./...
```

Tests need nothing but Go, at the version `go.mod` names: they run against this repository's own emulators of GitHub and Google Cloud.

Licensed under the [MIT License](LICENSE).
