# Images

A machine that has to download and install a lot before its job can start boots from an image that has it already. The service makes images on request, from definitions in the consumer's repository, and a job names the one it boots from in its label (specs/0002-machines.md). Images are made only by the service, from code on a consumer's default branch, so a pull request can boot from an image and ask for one to be made, but never change what goes into it.

## Base images

The service's own images, one per architecture, `lsr-base-<amd64|arm64>-<key>`: Ubuntu 26.04's current image, the GitHub runner at the version in `config/runner.yaml`, checked against its published SHA-256, the job-started hook, and git, curl, jq, python3, zstd and pigz. The key covers the Ubuntu image's name, the runner version and the text of what installs them. A deploy that changes the key starts a build of the new one, and machines keep booting the previous one until it's ready. Base images are what machines without an image boot, and what every consumer image is made from.

## Consumer images

A definition is a file in the consumer's repository, `.github/runners/images/<name>.yaml`:

```yaml
architecture: amd64
builder: lanescape-8vcpu-linux-amd64
disk_gb: 100
service_account: app-image@lanescape-dev.iam.gserviceaccount.com
script: ci/image.sh
key_files: [ci/image.sh, ci/lib.sh]
inputs: [version]
```

Everything the builder runs, the definition, the script and every file it uses, comes from one checkout of the consumer's repository at the `job_workflow_sha` of the request's token: the commit of the consumer's allowed workflow on its default branch, never the pull request's. Inputs are data, handed to the script in its environment, never code; the script decides what to check them against. One might refuse a version that isn't a published release, for example.

The definition's `architecture` must be the builder shape's, and the service account must be in the consumer's entry in `config/consumers.yaml`; a definition that breaks either is refused when it's requested. The service controls the builder's startup, so whatever that account can reach, the service can reach through it; a consumer gives it only what an image needs, such as reading what the image installs, and nothing that lets it replace what other builds read.

## Requesting an image

A consumer's job asks for an image with the images API, and waits for it:

```
POST /v1/images   {"definition": "build", "key": "<hex>", "inputs": {"tag": "...", "commit": "..."}}
GET  /v1/images/<name>
```

Both carry the job's GitHub OIDC token, which must be from the workflow the consumer allows on its default branch (specs/0001-architecture.md). The definition is the token's consumer's, and so is everything the request names; the image's name is `lsr-<namespace>-<definition>-<first 20 hex digits of a hash>` over the consumer's `key`, the definition's text, the contents of its `script` and its `key_files` at that commit, the base image's name and the inputs. `key_files` must list every file the script runs or reads that isn't already in the key, and the consumer's `key` covers what the image's content depends on beyond those, such as the versions of what it installs. A change to the script or anything it lists changes the name; a merge that changes none of them doesn't. A dependency left out of both is the consumer's mistake: changing it reuses the old image.

`POST` returns the image's name at once, and if the image is ready, marks it used. Otherwise the service makes sure exactly one build of it is under way. `GET` reports the build's state and the tail of its builder's log; the job prints it as it goes and finishes when the image is ready, or fails with the builder's log when the build fails.

## A build

A build is recorded in the service's bucket before anything is made: `images/<name>/<attempt>.json`, created only if absent, holding the request, the commit, the attempt, the builder's name, `lsr-<namespace>-build-<the name's hash>-<attempt>`, its zone, and the `requestId` for creating it. A pass that finds the record carries it on: it adopts a builder of that name in that zone if one exists, and otherwise retries the create with the same `requestId`, which Compute Engine treats as the same request, until it knows the outcome. Only a definite refusal, such as no capacity, moves it to another zone, updating the record first. A builder that has to be replaced, because it was preempted, first gets the replacement written into the record: the old builder's name and instance ID, and the new builder's name, `-<attempt>-<replacement>`, its provisioning model and a fresh `requestId`. Only then is the old one deleted and the new one created, and a pass that finds a pending replacement carries on with whichever of those two steps isn't done. So a create whose answer was lost doesn't make a second builder, a replacement is a new request, and a record whose maker died is still carried out.

1. The builder boots the base image, with the definition's shape and service account. Its startup script, the service's, first clears `lanescape-runners/safe-to-image`, then fetches the repository at the recorded commit with the App's token for that consumer, valid only for reading its contents, and runs the script as root. No GitHub job runs on a builder, so nothing but the service's startup script and the consumer's default-branch code ever touches its disk.
2. If the script succeeds, the startup script removes what a machine booted from the image mustn't share: the account's cached tokens, the App token, shell histories, SSH host keys, the machine id and cloud-init's record of the machine. It trims the disk and syncs the file systems, and only if all of that succeeded, as its very last act before powering off, sets the guest attribute `lanescape-runners/safe-to-image` to the machine's instance ID. A machine stopped any other way, before or during that cleanup, has no such attribute. A guest attribute is a signal from the service's own startup script, not proof: anything running on the builder could set it, and only the consumer's default-branch code does.
3. A builder is never started again once it has stopped: one that boots a second time, after a host error, say, ends its attempt. The service makes the image from the stopped disk only if that attribute names the machine's own instance ID, waits until the image is ready, labels it with its consumer, definition and `last-used=<UTC yyyymmdd>`, and deletes the builder and its disk.

A preempted builder is replaced within the same attempt, on-demand. A failed script, a builder that didn't power off within its limit, or one that stopped without the attribute ends its attempt; one more attempt is made, then requests for that image fail, saying why, until the records expire after 14 days or a change asks for a different image.

## Expiry

Booting a machine from an image, or a request that finds it ready, marks it used, at most once a day. Images unused for 90 days are deleted, except the newest ready image of each definition that a run on the consumer's default branch requested, as its token's `ref` and `event_name` show, recorded in the bucket once that image is ready. A pull request's request can come from the same workflow, but its token's `ref` is the pull request's. A base image goes once no consumer image made from it remains and a newer base image is ready.
