# Infrastructure

The service's own resources (specs/0004-deployment.md, CI and CD):

- the service on Cloud Run, `lanescape-runners`, adopted from the shell `bootstrap/` makes, scaled to zero, with a 10-minute request timeout, one request at a time per instance, CPU allocated outside requests (a supervisor's report starts a pass after its response), its environment and its secrets mounted from Secret Manager;
- the HTTP proxy on Cloud Run, `lanescape-runners-proxy`, adopted the same way;
- the proxy's git machine, `lsr-proxy-git`: a standard machine on Container-Optimized OS that runs the git server's image, with its mirrors on a persistent disk, `lsr-proxy-git-mirrors`, at a fixed internal address in the service's network;
- the buckets `lanescape-runners` and `lanescape-runners-proxy`, with public access prevention;
- the metadata of the App's two secrets, `lanescape-runners-app-key` and `lanescape-runners-webhook-secret`;
- who may use each of these, set on the resource itself.

The accounts, every project-level grant, the network, Pub/Sub, the scheduler job and the log sink are in `bootstrap/`, which only a maintainer applies.

## Who applies it

Only Infrastructure Manager, from CD, as `lanescape-runners-infra`, never anyone by hand. That identity can change nothing but these resources: the buckets, secrets, git machine and disks by name, and the two Cloud Run services by grants on each of them alone. It can't make or delete a Cloud Run service. What it holds that IAM can't limit is in `bootstrap/README.md`, What IAM can't limit. CD starts the apply, from this repository at the commit it has just built, with the images' digests:

```sh
gcloud infra-manager deployments apply \
  projects/lanescape-dev/locations/northamerica-northeast1/deployments/lanescape-runners \
  --service-account=projects/lanescape-dev/serviceAccounts/lanescape-runners-infra@lanescape-dev.iam.gserviceaccount.com \
  --git-source-repo=https://github.com/lanescape/runners.git \
  --git-source-directory=infra \
  --git-source-ref="$GITHUB_SHA" \
  --artifacts-gcs-bucket=gs://lanescape-runners-infra-manager \
  --tf-version-constraint=1.5.7 \
  --input-values="github_app_id=<App ID>,service_image=<image>@sha256:<digest>,proxy_image=<image>@sha256:<digest>,proxy_git_image=<image>@sha256:<digest>"
```

The source must come from the repository, not be uploaded: uploading needs project-wide Cloud Storage access that CD doesn't have. Each image must be in the `lanescape-runners` repository, by digest. Terraform here must work with 1.5.7, Infrastructure Manager's newest; CI validates it with that and the latest.

`check.sh`, which CI runs, refuses provisioners, `local-exec`, `external` data sources and modules here, any network or firewall rule here, more than the one firewall rule in `bootstrap/`, and any grant that replaces a policy rather than adding a member.

## What must exist first

- `bootstrap/`, applied, and what its README asks for before and after.
- The images, pushed by CD.

The first apply makes the App's secrets with no value, so the two Cloud Run services can't start. Add the values once, by hand, then let CD apply again:

```sh
gcloud secrets versions add lanescape-runners-app-key --data-file=app.pem
printf '%s' "$WEBHOOK_SECRET" | gcloud secrets versions add lanescape-runners-webhook-secret --data-file=-
```

A new version takes effect as instances start; nothing here needs changing.

## What the programs are given

- **The service:** `RUNNERS_AUDIENCE` (its own URL, which must be `endpoints.Service`, or it refuses to start), `RUNNERS_PROJECT` (the project its machines are in), `RUNNERS_BUCKET`, `RUNNERS_GITHUB_APP_ID`, `RUNNERS_GITHUB_APP_KEY_FILE`, `RUNNERS_WEBHOOK_SECRET_FILE` and `RUNNERS_RECONCILE_ACCOUNTS` (the scheduler's and the push subscriptions' accounts), as `cmd/runners` reads them; it listens on `PORT`, 8080.
- **The HTTP proxy:** `RUNNERS_AUDIENCE` (its own URL), `RUNNERS_PROXY_BUCKET` (its own bucket), `RUNNERS_BUCKET` (the service's, to check callers), `RUNNERS_GITHUB_APP_ID` and `RUNNERS_GITHUB_APP_KEY_FILE`; it listens on `PORT`.
- **The git server:** a container listening on `PORT`, 8443, published on the machine's port 443, with `RUNNERS_AUDIENCE` (the proxy's URL), `RUNNERS_BUCKET` and its mirrors in `RUNNERS_MIRRORS`, `/var/lib/proxy-git`, a directory owned by UID 65532. Its image must hold `git`.

The proxy's and the git server's names are this configuration's until `cmd/proxy` and `cmd/proxy-git` exist; change them here when those programs settle theirs.
