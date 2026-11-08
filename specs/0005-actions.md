# Actions

Consumers use the service through five actions in this repository, `lanescape/runners/actions/<name>@v1`. Each works inside the organization and degrades gracefully outside it, so the same workflow runs in a fork, at GitHub's expense, as far as it can without the service.

## Where an action is

Each action decides the same way, and says which in the job's summary:

- **Inside the organization:** `github.repository_owner_id` is the organization's, and either the run isn't from a fork's pull request, by the event's head repository, or it's a `pull_request_target` run, whose jobs the service accepts only once approved (specs/0001-architecture.md).
- **Outside:** anything else, including a fork's own pushes, another organization, and a fork's pull request into one of the organization's repositories, which runs in the base repository but never on the service's machines.

The decision is a courtesy, not a guard. The service and the proxy check everything for themselves, from GitHub's records and signed tokens.

## `runners`

Makes the labels a workflow's jobs run on. A consumer lists the runners it uses in `.github/runners.json`, each with its fallback for outside the organization:

```json
{
  "lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
  "lanescape-2vcpu-linux-amd64": "ubuntu-24.04"
}
```

A workflow's first job runs `runners` on one of GitHub's own runners, since no label is known yet, and every other job takes its label from that job's output. The action reads `.github/runners.json` from the run's own commit through GitHub's API, with the job's token, so it needs no checkout; in a `pull_request_target` run, that's the default branch's. Inside the organization it also binds the run's scope with the service (specs/0002-machines.md), so it must be the run's first job, with `contents: read` to read the file and `id-token: write` to sign in. In a `pull_request_target` workflow, the job instead calls the consumer's bootstrap workflow, which runs this action and nothing else, and whose reference the consumer names in `.github/runners.json` under `"bootstrap"`:

```yaml
jobs:
  runners:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      id-token: write
    outputs:
      map: ${{ steps.runners.outputs.map }}
    steps:
      - id: runners
        uses: lanescape/runners/actions/runners@v1
        with:
          disks: '{"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "key": "3f9c2e7a10b4d5e6f708"}}'
  build:
    needs: runners
    runs-on: ${{ fromJSON(needs.runners.outputs.map)['build'] }}
```

The output `map` has an entry for each name in `.github/runners.json`, and one for each entry in `disks`, named by its family:

- **Inside the organization,** each runner name maps to its selector for this run, with the run's scope: `lanescape-80vcpu-linux-amd64-pr42`. Each disk entry maps to its runner's selector with `-spot` if asked and `-disk-<family>-<key>`: `lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42`. The action works out the scope from the event as the service does, and the service checks it (specs/0002-machines.md).
- **Outside,** each runner name maps to its fallback, and each disk entry to its runner's fallback. A fork can override any fallback with a repository or organization variable `RUNNERS`, a JSON object in the same shape as `.github/runners.json`, whose values may be a label or a list of labels. Names it doesn't override keep the file's fallback, and names the file doesn't list are ignored. For a fork's pull request into an organization repository, the action ignores `RUNNERS` from the base repository, since it's the organization's.

A job that asks for a name the map lacks gets an empty label, which GitHub refuses; the action's summary lists the names it has.

## `checkout`

Checks out the repository for a job. With no inputs, it checks out the run's own commit, as `actions/checkout` does. With `contribution: true`, in a `pull_request_target` run, it checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, from the head repository, with no credentials persisted, and refuses in any other event. Outside the organization, `contribution: true` does the same in a fork's own `pull_request_target` runs, which use nothing of the service's.

## `save`

Asks for the job's work disk to be saved when it finishes. It writes the request on the boot disk, where the supervisor reads it once the runner has exited, outside any job; the service then saves the disk if the job succeeded and its scope and selector allow (specs/0003-disks.md). Outside the organization, or on a machine that isn't the service's, it does nothing and says so.

## `proxy`

Points a job's reads of upstream sources at the service's proxy (specs/0006-proxy.md). It starts the local forwarder on the machine, if it isn't running already, and configures git to fetch the consumer's allowlisted git upstreams through it. Where the proxy won't serve the job, it does nothing, and reads go straight to upstream: outside the organization, and on runners other than the service's for a `pull_request_target` run, whose GitHub token the proxy refuses. Tools other than git use `fetch`, or the forwarder's address, which the action sets as `LANESCAPE_PROXY`.

## `fetch`

Downloads a URL through the proxy to a file, with an optional expected SHA-256, which it checks before writing the file. Outside the organization, or for an upstream the consumer hasn't allowlisted, it downloads straight from upstream, and still checks the SHA-256.

## How actions run

Each action is a composite action that builds its Go program from the action's own source with `go build`, run in the repository root of the action's own downloaded copy, `$GITHUB_ACTION_PATH/../..`, into `$RUNNER_TEMP`, and runs the binary. What runs is exactly the code at the commit the action was referenced at, with no separate release to keep in step, and the job sees the program's own exit status, which `go run` wouldn't give. The base image has Go at the version `go.mod` names; elsewhere, the action sets up that version with `actions/setup-go` from `go.mod`. Builds use Go's build cache, so later steps of a job build in moments. The programs are pure Go, with CGO off, and CI builds them for every platform runners use: Linux on amd64 and arm64, macOS on arm64, and Windows on amd64.

## Versions

Consumers reference `@v1`, a tag that moves. Only CD moves it, to the commit it has just deployed, after that commit's tests pass, and only forward: the new commit must descend from the one `v1` names. CD runs one at a time on main, so two deploys never race to move it.

GitHub's rulesets name who may bypass them by account or App, not by workflow, and the ordinary `GITHUB_TOKEN` of any workflow acts as the same GitHub Actions App. So the tag is moved by a GitHub App of its own, "Lanescape Runners Release", installed only on this repository, with Contents write and nothing else. Its private key is in Secret Manager, and only CD's identity, which only a push to main from `cd.yml` can sign in as (specs/0004-deployment.md), may read it. A tag ruleset stops anyone but that App creating, updating or deleting `v*` tags, so moving `v1` takes the same review as merging to main. That matters because `checkout`, `save`, `proxy` and `fetch` run inside consumers' own jobs, beside whatever those jobs hold: whoever can merge here can change code in every consumer's jobs, which is the risk of a moving tag.

`v1` stays compatible: its actions keep their inputs, outputs and behaviour, and keep working against the service. A change that would break them comes as `v2`, with the service serving both until no consumer references `v1`.
