# Build tools

The bucket `gs://lanescape-ci-tools` holds the organization's build tools: platform-specific tools, SDKs and other files that any of its repositories' builds may copy onto their runners, whichever runners those are. Some of what it holds may never be published or redistributed, such as Apple's macOS SDK, so it's private, and nothing outside the organization can read it. It's defined in `tools/`, a Terraform root module of its own, which the maintainer applies by hand.

It's no part of the service. The service never reads it, and none of the service's accounts has any access to it: not the service's, not the machines' (which has no role anywhere, specs/0002-machines.md), not CD's deployer and not the infra identity. A job on the service's machines reads it as a job anywhere else does, with GitHub's token (Reading, below), never with its machine's account. `tools/` keeps its own state, under the prefix `tools` in `bootstrap/`'s state bucket, `lanescape-runners-tfstate`, and shares no resource with `bootstrap/` or `infra/`: applying, changing or destroying either of them leaves the bucket and its reader as they are, and the other way round.

## The bucket

`lanescape-ci-tools`, in project `lanescape-dev`, region `northamerica-northeast1`, as `bootstrap/` has them: standard storage, uniform bucket-level access, so no object has an ACL, public access prevention enforced, and Cloud Storage's default soft delete. Its only grant is the reader's (Reading, below). No grant on it, in `tools/`, may name everyone, signed in or not, nor may it have ACLs or relax public access prevention: `infra/check.sh`, which CI runs, refuses all of those (specs/0004-deployment.md).

The project is shared with other teams, and `tools/`'s grant to the reader isn't the only access to the bucket. Whoever holds a role on the project that reads Cloud Storage's objects, such as `roles/storage.objectViewer`, can read it, and IAM on a bucket can't take that away. And Cloud Storage gives every new bucket grants on the project's basic roles: its Owners and Editors may read and write its objects, and its Viewers, through `roles/storage.legacyObjectReader`, may read them. `tools/` adds members and replaces no policy, so it keeps those grants. That reaches the project's own members and no one else.

## Layout

```
<platform>/<tool>/<version>/<file>
<platform>/<tool>/<version>/<file>.sha256
```

`<platform>` names the system the files are for, such as `macos`; `<tool>`, the tool or SDK; `<version>`, its version. Each file has its SHA-256 beside it, in `<file>.sha256`, as `shasum -a 256` or `sha256sum` writes it, `<hex digest>  <file>`, with the file's name alone, so that `shasum -a 256 -c <file>.sha256` checks a copy in the same directory. For example:

```
macos/sdk/26.2/MacOSX26.2.sdk.tar.zst
macos/sdk/26.2/MacOSX26.2.sdk.tar.zst.sha256
```

## Objects never change

An object, once uploaded, is never replaced or changed: a new version of a tool, or a new build of one, gets a new path. So a build that names a path always gets the same bytes, and the `.sha256` it reads beside a file stays that file's. The maintainer uploads each object with a precondition that it doesn't exist yet, so an upload to a path already taken fails rather than replace it (`tools/README.md`), and uploads a file before its `.sha256`, so that a `.sha256` names only a file that's complete. The maintainer may delete a version that no build uses any more; a build that still names it then fails, rather than getting something else.

## Uploading

Only the maintainer uploads, with their own account, by hand (`tools/README.md`). Nothing in `tools/` can write to the bucket: there's no writer account, and no job of any repository can write to it. The project's Owners and Editors could, through the grants Cloud Storage gives a new bucket (The bucket, above); only the maintainer does.

## Reading

A job of any of the organization's repositories reads the bucket by signing in with GitHub's OIDC token, through the workload identity pool `lanescape-ci-tools` and its provider `github`, as the service account `lanescape-ci-tools-reader`, which holds `roles/storage.objectViewer` on this bucket alone, and nothing else anywhere:

- provider: `projects/168731703350/locations/global/workloadIdentityPools/lanescape-ci-tools/providers/github`
- account: `lanescape-ci-tools-reader@lanescape-dev.iam.gserviceaccount.com`

These don't change, so consumers may write them into their workflows. The job needs `permissions: id-token: write`; it signs in with `google-github-actions/auth`, given the provider as `workload_identity_provider` and the account as `service_account`, then copies a file and its `.sha256` with `gcloud storage cp`, or any Cloud Storage client, and checks the file with `shasum -a 256 -c` before using it.

The provider trusts tokens only from GitHub's issuer, `https://token.actions.githubusercontent.com`, and its condition accepts a token only if:

- its `repository_owner_id` is the organization's ID, 303994381, as `config/organization.yaml` has it, never its name. GitHub signs the token and sets that claim to the ID of the owner of the repository the run is in, which no workflow can choose, so a renamed or look-alike organization never matches, and a fork's own runs carry the fork's owner's ID;
- its `event_name` isn't `pull_request_target`.

Only principals with the organization's ID in `attribute.repository_owner_id` may act as the reader (`roles/iam.workloadIdentityUser`). The pool's subject is the run, `<repository ID>/<run ID>/<run attempt>`, so audit logs name the run that read, and a long branch's name can't push a token's `sub` past the subject's 127 bytes.

**Forks get nothing.** A fork's own runs carry the fork's owner's ID, which the provider refuses. A fork's `pull_request` run into one of the organization's repositories carries the organization's ID, and gets no OIDC token from GitHub, whatever its workflow asks for, so long as GitHub sends it no write tokens. GitHub never does for a public repository; for a private one, it does only if the organization's policy for fork pull requests in private repositories turns on "Send write tokens to workflows from pull requests". That policy must stay off: the provider can't tell a fork's `pull_request` token from one of a branch of the repository's own, so with it on, a fork's code could read the bucket. Today the organization doesn't run fork pull requests' workflows in private repositories at all, and sends them no write tokens (`gh api /orgs/lanescape/actions/permissions/fork-pr-workflows-private-repos`); the maintainer checks it before applying `tools/` (`tools/README.md`) and never turns it on.

**`pull_request_target` is refused.** A `pull_request_target` run does get a token, of the organization's repository, from the default branch's workflow, while its jobs may check out and run a fork's code: an approved contribution's jobs do (specs/0001-architecture.md, Forks and contributions). The token can't tell those jobs from the run's others, and anything a contributor's code could read it could publish, so no `pull_request_target` run signs in here, approved or not. A contribution's jobs get their tools elsewhere, or not at all.

The condition refuses no other event. A run of any other event runs a workflow from the organization's own repositories, which only those with write access to one can change; a workflow that runs code from outside them, such as a `workflow_run` workflow that runs what a fork's run produced, must not sign in here.

## Applying

The maintainer applies `tools/` by hand, with their own credentials, as `bootstrap/` is, after `bootstrap/`'s state bucket exists, and again whenever it changes (`tools/README.md`). Nothing applies it automatically: CD applies only `infra/`. CI checks it on every pull request, as it does `bootstrap/` and `infra/` (specs/0004-deployment.md, Tests). It enables the APIs it needs, `cloudresourcemanager`, `iam`, `iamcredentials`, `storage` and `sts`, which `bootstrap/` enables too; both leave them enabled when they're removed or destroyed, since other teams, and each other, use them.
