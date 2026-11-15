# Build tools

The organization's private bucket of build tools, `gs://lanescape-ci-tools`, and who may read it (specs/0006-tools.md). It's no part of the service: the service never reads it, and none of its accounts has any access. The maintainer applies it by hand, with their own credentials, and again whenever it changes. Nothing applies it automatically.

It holds:

- the APIs it needs, which stay enabled if they're removed from here, since `bootstrap/` and other teams use them too;
- the bucket, `lanescape-ci-tools`: private, with public access prevention enforced and uniform bucket-level access;
- the workload identity pool `lanescape-ci-tools`, whose provider `github` accepts GitHub's tokens only from the organization's repositories, by its ID, and never from a `pull_request_target` run;
- the reader, `lanescape-ci-tools-reader`, with `roles/storage.objectViewer` on the bucket alone, which the pool's principals of the organization may act as.

There's no writer: only the maintainer uploads, with their own account (below). The project's own members can read it too, through their roles on the project (specs/0006-tools.md, The bucket). `infra/check.sh`, which CI runs, refuses any grant here to everyone, signed in or not, any ACL, and any bucket without public access prevention enforced and uniform bucket-level access.

## What must exist first

- **Your access:** Owner of `lanescape-dev`, or enough to enable APIs, make buckets, workload identity pools and service accounts, and grant on a bucket and a service account.
- **No write tokens for forks' pull requests.** The organization's policy for fork pull requests in private repositories must never send write tokens to their workflows, or a fork's `pull_request` run could get a token this configuration accepts (specs/0006-tools.md, Reading). Check it before each apply; `send_write_tokens_to_workflows` must be `false`:

  ```sh
  gh api /orgs/lanescape/actions/permissions/fork-pr-workflows-private-repos
  ```

- **The state bucket,** `lanescape-runners-tfstate`, which `bootstrap/README.md` has the maintainer make before bootstrap's first apply. This configuration keeps its state there, under the prefix `tools`, apart from bootstrap's.

## Applying

```sh
cd tools
gcloud auth application-default login
terraform init
terraform plan -out=tools.tfplan
terraform apply tools.tfplan
terraform output
```

Read the plan before applying it: it's the only review this configuration's changes get beyond the pull request's. The outputs are what consumers write into their workflows, and must be exactly:

- `bucket`: `lanescape-ci-tools`
- `workload_identity_provider`: `projects/168731703350/locations/global/workloadIdentityPools/lanescape-ci-tools/providers/github`
- `reader`: `lanescape-ci-tools-reader@lanescape-dev.iam.gserviceaccount.com`

## Uploading a file

Objects never change (specs/0006-tools.md): a new version, or a new build of one, gets a new path, `<platform>/<tool>/<version>/<file>`. Upload the file first, then its `.sha256`, each with a precondition that the object doesn't exist yet, so an upload to a path already taken fails instead of replacing it. Write the `.sha256` in the file's own directory, so it names the file alone:

```sh
gcloud auth login
platform=macos tool=sdk version=26.2 file=MacOSX26.2.sdk.tar.zst
dest="gs://lanescape-ci-tools/$platform/$tool/$version"
shasum -a 256 "$file" > "$file.sha256" &&
  gcloud storage cp --if-generation-match=0 "$file" "$dest/$file" &&
  gcloud storage cp --if-generation-match=0 "$file.sha256" "$dest/$file.sha256"
```

Each step runs only if the one before succeeded, so a `.sha256` is never uploaded beside a file that wasn't.

Check what was uploaded by reading it back:

```sh
mkdir check && cd check
gcloud storage cp "$dest/$file" "$dest/$file.sha256" .
shasum -a 256 -c "$file.sha256"
```

If an upload fails because the object exists, never delete it to upload again: choose a new path. Delete a version only once no build uses it.
