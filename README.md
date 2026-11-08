# Lanescape Runners

GitHub Actions runners for the `lanescape` organization's repositories, on Google Cloud. A job asks for a machine by label, `runs-on: lanescape-80vcpu-linux-amd64`, and gets a Compute Engine machine of its own for that job alone, booted from a base image or from an image the repository defines. Jobs lost to Spot preemption are rerun, picking up the work they'd done when their work disk can be reattached. A job's machine has no cloud identity that can reach anything: jobs that need one sign in with GitHub's token.

This repository is at the design stage. The specs in [`specs/`](specs/) describe what it will do:

- [0001 Architecture](specs/0001-architecture.md): the service, consumers, identity and forks.
- [0002 Machines](specs/0002-machines.md): labels and shapes, a machine's life, the work disk and preemption.
- [0003 Images](specs/0003-images.md): base images, consumer images and the images API.
- [0004 Code, tests and deployment](specs/0004-deployment.md): Go, CI and CD on Blacksmith, and Terraform through Infrastructure Manager.

Licensed under the [MIT License](LICENSE).
