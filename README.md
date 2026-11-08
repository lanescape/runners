# Lanescape Runners

GitHub Actions runners for the repositories of the `lanescape` organization, and for no one else, on Google Cloud. A job asks for a machine by label, as it would ask Blacksmith or GitHub, and gets a Compute Engine machine of its own for that job alone. A job can save its work disk when it finishes, and later jobs of the same pull request, or of main, start from it. Jobs read upstream sources through a caching proxy. A job's machine has no cloud identity that can reach anything: jobs that need one sign in with GitHub's token.

The code is public so that anyone can read it and contribute, but the service itself serves only the `lanescape` organization. Its actions work anywhere: outside the organization they fall back to GitHub's runners, or to a fork's own, and read straight from upstream.

This repository is at the design stage. The specs in [`specs/`](specs/) describe what it will do:

- [0001 Architecture](specs/0001-architecture.md): the service, the organization-only guards, endpoints, passes, forks and contributions.
- [0002 Machines](specs/0002-machines.md): selectors, scopes, a machine's life, the work disk and preemption.
- [0003 Images and saved disks](specs/0003-disks.md): base images, saving and restoring work disks, and expiry.
- [0004 Code, tests and deployment](specs/0004-deployment.md): Go, our own emulators, CI and CD on Blacksmith, and Terraform through Infrastructure Manager.
- [0005 Actions](specs/0005-actions.md): `runners`, `checkout`, `save`, `proxy` and `fetch`, and the `v1` tag.
- [0006 Proxy](specs/0006-proxy.md): cached reads of HTTP and git upstreams.

Licensed under the [MIT License](LICENSE).
