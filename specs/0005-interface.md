# The interface for actions

Consumers' workflows use the service through its selectors, their own `.github/runners.json`, and actions maintained in their own repository, `lanescape/actions`, which call this service the way many actions call an API maintained elsewhere. This spec is everything those workflows and actions may rely on. It's versioned as `/v1/`, and stays compatible: a change that would break an action comes as `/v2/`, beside `/v1/`, which goes only once no action uses it (specs/0001-architecture.md). That promise starts with the service's first release. Until then nothing uses `/v1/`, and it may still change, as it has: runs are no longer bound by `POST /v1/bind`, selectors no longer carry a scope, a run, an image or a disk, and a job asks for its image and its work disk in its steps.

## Where the service is

The service's address is a constant of this module's package `github.com/lanescape/runners/endpoints`, `endpoints.Service`, at the version an action's `go.mod` names, beside the organization's immutable ID, `endpoints.Organization`, which is what an action compares `github.repository_owner_id` with, never the organization's name. The address is also the audience of every token sent to it. Every request to the service carries its token as `Authorization: Bearer <token>`.

A token is a credential: the caller never logs one, nor puts one in an output or a file other jobs can read.

**On the service's machines,** Compute Engine's metadata server, `http://metadata.google.internal`, answers `computeMetadata/v1/instance/attributes/lsr-service` with the service's address, from the host and from a job's containers on Docker's default network. An action tells it's on one of the service's machines by that answer; anywhere else the name doesn't resolve, or the attribute is absent.

## Selectors

A job names its machine with one selector (specs/0002-machines.md, Selectors), written in the workflow:

```
lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot]
```

```yaml
runs-on: lanescape-80vcpu-linux-amd64-spot
```

A selector is in lowercase. The shapes the service offers are in `config/shapes.yaml`; the smallest is `lanescape-2vcpu-linux-amd64`. Nothing runs to make selectors, and nothing of the run is in them: the service binds every run itself, at its first job's check, from what GitHub told that job's runner, confirmed against GitHub's records (specs/0002-machines.md, Binding a run). A `pull_request_target` run is bound only once a maintainer has approved its `contribution` environment, so a consumer that runs approved contributions gives such a run one contribution job: the only job that names that environment, which every other job of the run on the service's machines needs, and which runs only the default branch's code, on the smallest shape, standard, with no image and no work disk. It needs no step of ours. The service takes no GitHub token, so a contributor's code can't bind anything with one; **every party that does trust the consumer's OIDC tokens must tell one whose `event_name` is `pull_request_target` apart, and give it only what the consumer means a contribution to have, or refuse it** (specs/0001-architecture.md, Forks and contributions).

A job's `runs-on` is exactly its selector, and nothing else: GitHub gives a job only a runner that has every label it names, and the service's runners have their selector alone. Outside the organization no runner has these labels, so a job that names one waits for a runner until GitHub gives up. A workflow that also runs in forks skips such jobs there, or chooses another label, by expressions on `github.repository_owner_id`, for a fork's own runs, and on `github.event.pull_request.head.repo.id` against `github.repository_id`, for a fork's `pull_request` run into the consumer, which runs in the consumer's repository and gets none of the service's machines. An approved contribution's `pull_request_target` run has a fork's head too, but does run on the service's machines, so the workflow tests `github.event_name` as well.

## A consumer's files

**`.github/runners.json`** is an object with any of three fields:

- **`"branches"`:** an object from each long-lived branch's name to the name of its scope, 1 to 16 lowercase letters and digits, starting with a letter: `{"support/v1": "v1"}` gives pushes to `support/v1` the scope `branch-v1` (specs/0002-machines.md, Scope). The service reads it only from the default branch, never a run's own copy.
- **`"disks"`:** an object from each family a `disk` request names to its declaration, `{"from": [<path>, ...]}` or `{}`, from which the service makes the family's key (specs/0003-disks.md, Keys).
- **`"images"`:** an object from each image an `image` request names to the directory that defines it (specs/0006-images.md).

An image's name and a family are 1 to 32 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen. The service reads `"disks"` and `"images"`, and the files they name, from the run's commit (specs/0002-machines.md, Binding a run), so a pull request that adds a family or changes an image uses it in its own runs. Any other field, or a field named in another case, makes the file invalid, and a request that relies on it is refused, saying why.

**An image's directory** holds `image.json`, `init.sh`, optionally `check.sh`, and the files they read (specs/0006-images.md, Defining an image).

## Requests from a job

From a job on one of the service's machines, signed with the machine's identity token: a Compute Engine identity token in the full format, audience `endpoints.Service`, from the machine's metadata server, got afresh for each request, from the host or from the job's container. The service answers only a machine it made whose job it admitted, while that job runs, only for that job and its run, and only once the run is bound: an unbound `pull_request_target` run's job gets `403`. Every request below answers `403` with `{"reason": "<why>"}` for a request the service refuses, and `503` with `{"retryable": true}` and a `Retry-After` when GitHub couldn't confirm an answer, which the caller retries; every request is safe to repeat.

## Images

**`POST /v1/images`** asks for an image of the run's commit, with the body `{"name": "<image>"}`. The answer is `200` with:

```json
{"version": "<20 hex>", "state": "ready" | "building" | "failed", "ref": "<reference>", "reason": "<why>",
 "builds": [{"architecture": "amd64", "state": "ready" | "building" | "failed", "build": "<id>", "reason": "<why>"}]}
```

`ready` if the run's restore chain has the version, or its builds have just made it, with `ref`, the image's reference, `<registry path>/<repository ID>/<scope>/<name>@sha256:<digest>` (specs/0006-images.md, The image's reference), which a job names in `container:`; `building` while any architecture's build is under way, with the ID of each build it started or joined; `failed`, with the reason, as soon as one has failed, or if the image can't be ready before the asking machine's age limit. `builds` has one entry for each of the image's architectures; `build` is absent for an architecture whose image the restore chain already had, and `reason` for anything but a failure. A request is refused for an image the run's commit doesn't declare, or a definition that isn't valid. A run's first `ready` answer for a name fixes its image: every later request of the run for that name, in any attempt, gets the same `ref` while that image exists.

**`GET /v1/images/builds/<id>?offset=<bytes>`** answers `200` with `{"state": "building" | "ready" | "failed", "reason": "<why>", "log": "<text>", "next": <bytes>}`: the build's log from `offset`, as UTF-8 text with anything else replaced, at most 1 MiB of it, and the offset to ask from next. Only a machine of a run that asked for the build, by starting or joining it, may ask; any other gets `403`.

A build's log is the output of the consumer's own scripts. A job that prints it prints it into its own run's log, which is as public as the repository.

**Pulling.** A job names an image's `ref` in `container:`, or `container.image`, with no `credentials`: the machine's Docker pulls it with a pull token the service gives the machine for its admitted job (specs/0002-machines.md, Pulling images). The reference is pinned by its digest, so a job runs exactly the image its run was answered with.

## Work disks

**`POST /v1/disks`** asks for the job's work disk, with the body `{"family": "<family>"}`. The answer is `200` with `{"state": "making" | "ready", "restored": "<step>"}`: `making` while the service makes and attaches it, which the caller asks again for, every 5 seconds, until `ready`, once Compute Engine shows it attached to the machine; `restored` says where it came from, `resume` for a resume image, or the step of the restore order, `pull_request_exact`, `pull_request_newest`, `main_exact`, `main_newest`, `branch_exact`, `branch_newest` or `none` (specs/0003-disks.md). The scope is always the one the service bound the job's run to; the family is the only thing the job chooses. A request is refused for a family `.github/runners.json` at the run's commit doesn't declare, a file it lists that doesn't exist, a file that isn't valid, or another family than the job's work disk already has: a job has one work disk.

Once the answer is `ready`, the machine mounts the disk at `/work`, on the host, outside the job (specs/0002-machines.md, The work disk). A caller waits until `/work`, as its own process sees it, is that mounted disk: on the host, or in a container that maps `/work:/work:rslave`, which every container job that uses the disk does. Before that, `/work` is an empty directory owned by root.

**`POST /v1/disks/save`** asks for the job's work disk to be saved when the job finishes, as the `save-disk` action does, with the body `{"save": true}`. The answer is `200` with `{"saves": true}` if the run's scope saves, so the disk is saved if GitHub's conclusion for the job is success, or `{"saves": false, "reason": "<why>"}` if it doesn't, such as in the `read` scope. A request from a job with no work disk is refused. Whether the disk is saved is still the service's to decide, once the job has finished (specs/0003-disks.md); the job learns nothing more of it, since it has ended. Nothing withdraws a save once asked.

## The machines

Jobs on the service's machines run as the runner's user, with passwordless sudo and Docker, as on GitHub's runners, and find their work disk at `/work` once their `disk` step has asked for it (specs/0002-machines.md). A job the service can't serve is stopped before its first step and before its containers start, with the reason in its log, and its run is cancelled; none of its steps run, whatever their conditions (specs/0002-machines.md, Failing a job). Some of what this relies on is to be confirmed on real GitHub at the first deploy (specs/0002-machines.md, To confirm on real GitHub).

## Testing against the emulator

Consumers test their actions against an emulator of this interface rather than the service: the package `github.com/lanescape/runners/emulators/service`, at the version of this module their `go.mod` pins. It implements the requests this spec describes, `/v1/images`, `/v1/disks` and `/v1/disks/save`, and nothing else, and binds runs as the service does. It runs in-process as an `httptest` server and verifies machine identity tokens against its own issuer, as the service verifies Compute Engine's. A test drives it through its `World`: it declares a repository, its files at each commit, its branches, pull requests, from the repository or a fork, and pushes to them, and runs and jobs as GitHub would show them, with their environments and approvals; it starts a job as if on one of the service's machines, which binds the job's run by the rules of specs/0002-machines.md from what GitHub shows of the job, as the service's check does, and mints that machine's identity token; and the emulator resolves images by specs/0006-images.md and chooses disks by specs/0003-disks.md. A test decides how each build goes, holding it, finishing it, failing it, and writing its log, and how each disk goes, holding it at `making` or making it `ready`, and reads what was bound, built, attached and asked to be saved. It finishes jobs, reruns runs as a maintainer, makes GitHub unavailable or adds any other failure through `emulators/fault`, with `Refuse` and `Unavailable` answering 403 with a reason and 503 with a `Retry-After` as the service does. It doesn't mount anything: what a caller sees at `/work` is the test's to arrange.

**One GitHub for both.** A consumer's test that also runs this module's GitHub emulator, `emulators/github`, to give its jobs the API, event and tokens they see, hands that emulator to the service emulator, `service.New(service.WithGitHub(gh))`, and the service emulator takes its view of GitHub from it, in-process, in place of a model of its own: the repositories of the organization the test makes there, their branches, commits and files, their pull requests and each one's commits, runs and jobs, environments and approvals, as they are when each request arrives; so a job started there is bound from the refs, commits, events and approvals GitHub's emulator gave, as the service's check would bind it, and nothing is translated between two GitHubs. The test may change anything in `gh` directly, such as pushing to a pull request's head branch and syncing it, approving a run, finishing a job or closing a pull request; the emulator sees it at its next request or `Binding`. `World`'s methods act on `gh` too; `RepositoryOf`, `PullRequestOf`, `RunOf` and `JobOf` give the handles of what the test made in `gh` itself, and `GitHubRepo`, `GitHubPull`, `GitHubRun` and `GitHubJob` the objects behind a handle. Faults added to `gh`'s API don't reach the emulator, which doesn't call it; `Unavailable` makes GitHub unavailable to it.

The conformance suite, the public package `github.com/lanescape/runners/conformance`, is this repository's promise that the emulator is the service as far as this interface goes. Its cases run in this repository's CI against the emulator, with a model of GitHub of its own and on a GitHub emulator, and against the real service wired to this repository's emulators of GitHub, Compute Engine, Artifact Registry, Google's issuers, Pub/Sub and Cloud Storage, and each must pass every one: the scopes runs bind, a binding made once and kept by a maintainer's rerun, a tag named like a branch, forks, approved contributions and their contribution job, one pushed to after its approval, a contribution bound to its own pull request only, a run not approved left unbound and refused images and disks, a job on a machine made for another run's job, image requests from a run's own machine and refused from any other, a build started, joined, finished and failed, a version from the run's commit and its restore chain, a run's `ref` kept across its attempts, a work disk made in the run's own scope from each step of the restore order, a second family refused, a save asked for with and without a work disk and in a scope that saves nothing, and the shapes of every refusal and retry. A deliberate divergence of either side's answers fails it, which the suite's own tests check. A change to the service that fails a case breaks `/v1/`, and a change to the emulator that fails one breaks consumers' tests.
