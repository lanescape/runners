# The interface for actions

Consumers' workflows use the service through its selectors, their own `.github/runners.json`, and actions maintained in their own repository, `lanescape/actions`, which call this service the way many actions call an API maintained elsewhere. This spec is everything those workflows and actions may rely on. It's versioned as `/v1/`, and stays compatible: a change that would break an action comes as `/v2/`, beside `/v1/`, which goes only once no action uses it (specs/0001-architecture.md). That promise starts with the service's first release. Until then nothing uses `/v1/`, and it may still change, as it has: runs are no longer bound by `POST /v1/bind`, and selectors no longer carry a scope.

## Where the service is

The service's address is a constant of this module's package `github.com/lanescape/runners/endpoints`, `endpoints.Service`, at the version an action's `go.mod` names, beside the organization's immutable ID, `endpoints.Organization`, which is what an action compares `github.repository_owner_id` with, never the organization's name. The address is also the audience of every token sent to it. Every request to the service carries its token as `Authorization: Bearer <token>`.

A token is a credential: the caller never logs one, nor puts one in an output or a file other jobs can read.

## Selectors

A job names its machine with one selector (specs/0002-machines.md, Selectors), written in the workflow:

```
lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot][-image-<name>][-disk-<family>]-run-<run ID>
```

```yaml
runs-on: lanescape-80vcpu-linux-amd64-spot-image-build-disk-build-run-${{ github.run_id }}
```

The run ID is `github.run_id`. An image's name and a family are 1 to 32 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen, with no hyphen-separated word `spot`, `image`, `disk` or `run`; a selector is at most 200 characters, in lowercase. The shapes the service offers are in `config/shapes.yaml`; the smallest is `lanescape-2vcpu-linux-amd64`. Nothing runs to make selectors, and the run's scope isn't in them: the service binds every run itself, at its first job's check on the base image, from what GitHub told that job's runner, confirmed against GitHub's records (specs/0002-machines.md, Binding a run). A `pull_request_target` run is bound only once a maintainer has approved its `contribution` environment, so a consumer that runs approved contributions gives such a run one contribution job: the only job that names that environment, which every other job of the run on the service's machines needs, and which runs only the default branch's code, on the smallest shape with neither image nor disk. It needs no step of ours. The service takes no GitHub token, so a contributor's code can't bind anything with one; **every party that does trust the consumer's OIDC tokens must tell one whose `event_name` is `pull_request_target` apart, and give it only what the consumer means a contribution to have, or refuse it** (specs/0001-architecture.md, Forks and contributions).

A job's `runs-on` is exactly its selector, and nothing else: GitHub gives a job only a runner that has every label it names, and the service's runners have their selector alone. Outside the organization no runner has these labels, so a job that names one waits for a runner until GitHub gives up. A workflow that also runs in forks skips such jobs there, or chooses another label, by expressions on `github.repository_owner_id`, for a fork's own runs, and on `github.event.pull_request.head.repo.id` against `github.repository_id`, for a fork's `pull_request` run into the consumer, which runs in the consumer's repository and gets none of the service's machines. An approved contribution's `pull_request_target` run has a fork's head too, but does run on the service's machines, so the workflow tests `github.event_name` as well.

## A consumer's files

**`.github/runners.json`** is an object with any of three fields:

- **`"branches"`:** an object from each long-lived branch's name to the name of its scope, 1 to 16 lowercase letters and digits, starting with a letter: `{"support/v1": "v1"}` gives pushes to `support/v1` the scope `branch-v1` (specs/0002-machines.md, Scope). The service reads it only from the default branch, never a run's own copy.
- **`"disks"`:** an object from each family a selector names to its declaration, `{"from": [<path>, ...]}` or `{}`, from which the service makes the family's key (specs/0003-disks.md, Keys).
- **`"images"`:** an object from each image a selector names to the directory that defines it (specs/0006-images.md).

The service reads `"disks"` and `"images"`, and the files they name, from the run's commit (specs/0002-machines.md, Binding a run), so a pull request that adds a family or changes an image uses it in its own runs. Any other field, or a field named in another case, makes the file invalid, and a job that relies on it is failed, saying why.

**An image's directory** holds `image.json`, `init.sh`, optionally `check.sh`, and the files they read (specs/0006-images.md, Defining an image).

## Images

From a job on one of the service's machines, signed with the machine's identity token: a Compute Engine identity token in the full format, audience `endpoints.Service`, from the machine's metadata server. The service answers only a machine it made whose job it admitted, only for that job's run, and only once the run is bound: an unbound `pull_request_target` run's job gets `403`.

**`POST /v1/images`** asks for an image of the run's commit, with the body `{"name": "<image>"}`. The answer is `200` with `{"builds": [{"architecture": "amd64", "version": "<20 hex>", "state": "ready" | "building" | "failed", "build": "<id>", "reason": "<why>"}]}`, one entry for each of the image's architectures: `ready` if the run's restore chain has the version, or a build that has just succeeded; `building` with the ID of the build it started or joined; `failed` with the ID and reason of a build that failed. `403` with a reason answers a request the service refuses: not from one of its machines' admitted jobs, an image the run's commit doesn't declare, or a definition that isn't valid. `503` with `{"retryable": true}` and a `Retry-After` answers when GitHub couldn't confirm an answer, which the caller retries. A run's first request for an image records the version it resolved, for each architecture, against the base image of the time, and every later request, build and machine of the run uses that record, so a new base image partway through a run never changes what it boots.

**`GET /v1/images/builds/<id>?offset=<bytes>`** answers `200` with `{"state": "building" | "ready" | "failed", "reason": "<why>", "log": "<text>", "next": <bytes>}`: the build's log from `offset`, as UTF-8 text with anything else replaced, and the offset to ask from next. Only a machine of a run that asked for the build, by starting or joining it, may ask; any other gets `403`.

A build's log is the output of the consumer's own scripts. A job that prints it prints it into its own run's log, which is as public as the repository.

## Asking to save a work disk

On the service's machines, a job asks to save its work disk, as the `save-disk` action does, by writing `{"save": true}` to `/var/lib/lanescape-runners/save`, in a directory the runner's user may write, which the supervisor reads once the runner has exited (specs/0002-machines.md). The file is absent on other runners. Anything else in it, a link in its place, or no file at all, asks for no save. Whether the disk is saved is still the service's to decide, from GitHub's conclusion for the job and its run's scope (specs/0003-disks.md); the job learns nothing of it, since it has ended.

## The machines

Jobs on the service's machines run as the runner's user, with passwordless sudo, as on GitHub's runners, and find their work disk at `/work` (specs/0002-machines.md). A job the service can't serve as its selector asks is stopped before its first step, with the reason in its log, and its run is cancelled; none of its steps run, whatever their conditions (specs/0002-machines.md, Failing a job).

## Testing against the emulator

Consumers test their actions against an emulator of this interface rather than the service: the package `github.com/lanescape/runners/emulators/service`, at the version of this module their `go.mod` pins. It implements the requests this spec describes, `/v1/images`, and nothing else, and binds runs as the service does. It runs in-process as an `httptest` server and verifies machine identity tokens against its own issuer, as the service verifies Compute Engine's. A test drives it through its `World`: it declares a repository, its files at each commit, its branches, pull requests, from the repository or a fork, and pushes to them, and runs and jobs as GitHub would show them, with their environments and approvals; it starts a job as if on one of the service's base-image machines, which binds the job's run by the rules of specs/0002-machines.md from what GitHub shows of the job, as the service's check does, and mints that machine's identity token; and the emulator resolves images by specs/0006-images.md. A test decides how each build goes, holding it, finishing it, failing it, and writing its log, and reads what was bound and built. It finishes jobs, reruns runs as a maintainer, makes GitHub unavailable or adds any other failure through `emulators/fault`, with `Refuse` and `Unavailable` answering 403 with a reason and 503 with a `Retry-After` as the service does.

**One GitHub for both.** A consumer's test that also runs this module's GitHub emulator, `emulators/github`, to give its jobs the API, event and tokens they see, hands that emulator to the service emulator, `service.New(service.WithGitHub(gh))`, and the service emulator takes its view of GitHub from it, in-process, in place of a model of its own: the repositories of the organization the test makes there, their branches, commits and files, their pull requests and each one's commits, runs and jobs, environments and approvals, as they are when each request arrives; so a job started there is bound from the refs, commits, events and approvals GitHub's emulator gave, as the service's check would bind it, and nothing is translated between two GitHubs. The test may change anything in `gh` directly, such as pushing to a pull request's head branch and syncing it, approving a run, finishing a job or closing a pull request; the emulator sees it at its next request or `Binding`. `World`'s methods act on `gh` too; `RepositoryOf`, `PullRequestOf`, `RunOf` and `JobOf` give the handles of what the test made in `gh` itself, and `GitHubRepo`, `GitHubPull`, `GitHubRun` and `GitHubJob` the objects behind a handle. Faults added to `gh`'s API don't reach the emulator, which doesn't call it; `Unavailable` makes GitHub unavailable to it.

The conformance suite, the public package `github.com/lanescape/runners/conformance`, is this repository's promise that the emulator is the service as far as this interface goes. Its cases run in this repository's CI against the emulator, with a model of GitHub of its own and on a GitHub emulator, and against the real service wired to this repository's emulators of GitHub, Compute Engine, Google's issuers, Pub/Sub and Cloud Storage, and each must pass every one: the scopes runs bind, a binding made once and kept by a maintainer's rerun, a tag named like a branch, forks, approved contributions and their contribution job, one pushed to after its approval, a contribution bound to its own pull request only, a run not approved left unbound, image requests from a run's own machine and refused from any other, a build started, joined, finished and failed, a version from the run's commit and its restore chain, and the shapes of every refusal and retry. A deliberate divergence of either side's answers fails it, which the suite's own tests check. A change to the service that fails a case breaks `/v1/`, and a change to the emulator that fails one breaks consumers' tests.
