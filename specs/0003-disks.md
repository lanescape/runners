# Images and saved disks

Every machine boots the service's own base image. What a consumer's jobs need beyond it lives on their work disks, which a job can save when it finishes, so that later jobs start from it. A saved disk serves the run's scope and no other (specs/0002-machines.md): main's disks are saved only by pushes to the default branch, a pull request's only by its own runs, and nothing of a pull request's ever reaches main or another pull request.

## Base images

The service's own images, one per architecture, `lsr-base-<amd64|arm64>-<key>`: Ubuntu 26.04's current image, the GitHub runner at the version in `config/runner.yaml`, checked against its published SHA-256, the supervisor and the job-started hook, the proxy's local forwarder (specs/0006-proxy.md), and git, curl, jq, python3, zstd, pigz and Go at the version this repository's `go.mod` names. The key covers the Ubuntu image's name, the runner version, the Go version, the text of what installs them, and the digest of every program of the service's baked into the image, so a change to the supervisor, the hook or the forwarder alone makes a new image. When a deploy changes the key, the service makes the new one itself, on a builder machine that runs only the service's own script, and machines keep booting the previous one until it's ready. No consumer's code ever runs on a base image's builder, and no consumer can make or change a base image.

## Saved disks

A selector's `-disk-<family>-<key>` asks for a work disk made from a saved one. The service picks the first that exists of:

1. in a pull request's scope only, a disk saved in that pull request's scope for that family and key;
2. in a pull request's scope only, the newest disk saved in that pull request's scope for that family, whatever its key;
3. a disk saved in main's scope for that family and key;
4. the newest disk saved in main's scope for that family;
5. none: the work disk starts empty.

Families are kept apart by architecture: a family's disks saved from `amd64` machines are never restored on `arm64` ones, or the other way round. The newest disk of a family is how a job whose key has changed, such as a pull request that moves a dependency, still starts from the state nearest to its own and rebuilds only what changed; a consumer's job must check what it restored and rebuild what's out of date, since a key miss restores older state. Every job of a run that uses the same selector starts from the same choice: the service picks once per run and selector, the first time it admits a job with it, and records the exact image version in the bucket. Picking and recording a choice, and marking an image for deletion, each happen under a short lease of their own in the bucket, apart from the pass's, held only for that step, so no image is chosen as it's being marked, nor marked once chosen. An image is deleted only after it's marked, and an image marked for deletion is never chosen. Labels on images and records hold hashes of selectors, families and job names wherever a value could be longer or freer than a Compute Engine label allows; the bucket holds the values themselves.

A job asks to save its work disk with the `save` action (specs/0005-actions.md). When the job has finished, the service saves it only if:

- the job succeeded, by GitHub's conclusion for it;
- the run's scope saves: main's for a push to the default branch, a pull request's for that pull request's runs, never `read`;
- the job's selector names a family and key.

It makes an image of the stopped machine's work disk, `lsr-<repository ID>-<SHA-256 of scope, family and key, first 20 hex digits>`, labelled with the repository ID, and with hashes of the scope, family and key, since Compute Engine labels are short and allow few characters, and with `saved` and `last-used` dates. Saving again for the same scope, family and key makes a new version, named with its save time, which later restore choices pick. An older version is deleted once the new one is ready and no unfinished run's recorded restore choice names it. Only the work disk is saved, as the job left it. The runner's directory, with its checkouts and any credentials tools left beside them, is on the boot disk, which is never saved.

**Resume images** are the same kind of image, made from a preempted Spot machine's work disk, labelled `resume` with the run and hashes of the selector and the job's name, the full values being in the bucket's record of the preemption, and used only to resume that one job (specs/0002-machines.md). They're never a scope's saved disk, so a disk left mid-write by a preemption is never what a later run starts from.

## What reads what

Main's disks are read by every pull request of the consumer, approved contributions included, and by its `read` runs. A pull request's disks are read only by that pull request's later runs. A contributor's code can therefore change what its own pull request's later runs start from, and nothing else. A pull request's disk that main's code would want, such as a dependency it built for a new pin, is built again by main after the merge: that costs a second build, and in return main never starts from anything a pull request wrote.

## Expiry and limits

Starting a machine from a saved disk marks it used, at most once a day. A pull request's disks are deleted 14 days after their last use, or once the pull request has been closed for a day. Main's are deleted 90 days after their last use, except the newest of each family. Resume images go as specs/0002-machines.md says. No deletion of any kind removes an image version that an unfinished run's recorded restore choice names, or that a run still due an automatic rerun needs. A run's restore choices are dropped once it has finished and has no automatic rerun left; a later rerun of it, by a maintainer, picks again, as a new run would.

Each consumer has two limits on the total size of its saved disks, in `config/limits.yaml`: one for main's and one for all its pull requests' together. A save that would pass its scope's limit first deletes that scope's least recently used disks, never another scope's, nor the newest of a main family, nor a version an unfinished run's restore choice names. If that isn't enough, the save is skipped, and the job's log says why. So a pull request can never push main's disks out.
