# Infrastructure

The service's own resources (specs/0004-deployment.md, CI and CD):

- the service on Cloud Run, `lanescape-runners`, adopted from the shell `bootstrap/` makes, scaled to zero, with a 10-minute request timeout, one request at a time per instance, CPU allocated outside requests (a supervisor's report starts a pass after its response), its environment and its secrets mounted from Secret Manager;
- the bucket `lanescape-runners`, for the service's state, with public access prevention, and the service as the only identity granted on it;
- the metadata of the App's two secrets, `lanescape-runners-app-key` and `lanescape-runners-webhook-secret`;
- who may use each of these, set on the resource itself.

The accounts, every project-level grant, the network, Pub/Sub, the scheduler job and the log sink are in `bootstrap/`, which only a maintainer applies.

## Who applies it

Only Infrastructure Manager, from CD, as `lanescape-runners-infra`, never anyone by hand. That identity can change nothing but these resources: the bucket and secrets by name, and the Cloud Run service by a grant on it alone. It can't make or delete a Cloud Run service. What it holds that IAM can't limit is in `bootstrap/README.md`, What IAM can't limit. CD (`.github/workflows/cd.yml`) first asks for a preview, with the same source and inputs, and prints its logs, then starts the apply, from this repository at the commit it has just built, with the image's digest, and prints its logs too. It starts the apply through Infrastructure Manager's API itself, since gcloud's apply of an existing deployment first lists the project's shared default staging bucket, which CD has no grant on and a git source never uses; the request is the same as this:

```sh
gcloud infra-manager deployments apply \
  projects/lanescape-dev/locations/northamerica-northeast1/deployments/lanescape-runners \
  --service-account=projects/lanescape-dev/serviceAccounts/lanescape-runners-infra@lanescape-dev.iam.gserviceaccount.com \
  --git-source-repo=https://github.com/lanescape/runners.git \
  --git-source-directory=infra \
  --git-source-ref="$GITHUB_SHA" \
  --artifacts-gcs-bucket=gs://lanescape-runners-infra-manager \
  --tf-version-constraint=1.5.7 \
  --input-values="github_app_id=<App ID>,service_image=<image>@sha256:<digest>,deploy_id=<run ID>-<attempt>"
```

`deploy_id`, on the Cloud Run service's template, makes every apply a new revision, even of an image already deployed, so a rerun of a deploy whose revision couldn't start tries again. The source must come from the repository, not be uploaded: uploading needs project-wide Cloud Storage access that CD doesn't have. The image must be in the `lanescape-runners` repository, by digest. Terraform here must work with 1.5.7, Infrastructure Manager's newest; CI validates it with that and the latest.

`check.sh`, which CI runs, refuses provisioners, `local-exec`, `external` data sources and modules here, any network here, any firewall rule or policy here or in `bootstrap/`, and any grant that replaces a policy rather than adding a member.

## What must exist first

- `bootstrap/`, applied, and what its README asks for before and after.
- The image, pushed by CD.

CD deploys nothing until the repository's variables that `bootstrap/README.md` lists are set. The first apply makes the App's secrets with no value, so the service can't start, and the apply fails there. Add the values once, by hand, then rerun the failed CD run's deploy:

```sh
gcloud secrets versions add lanescape-runners-app-key --data-file=app.pem
printf '%s' "$WEBHOOK_SECRET" | gcloud secrets versions add lanescape-runners-webhook-secret --data-file=-
```

A new version takes effect as instances start; nothing here needs changing.

## What the programs are given

- **The service:** `RUNNERS_AUDIENCE` (its own URL, which must be `endpoints.Service`, or it refuses to start), `RUNNERS_PROJECT` (the project its machines are in), `RUNNERS_BUCKET`, `RUNNERS_WEBHOOK_TOPIC` (the topic the webhook publishes deliveries to), `RUNNERS_GITHUB_APP_ID`, `RUNNERS_GITHUB_APP_KEY_FILE`, `RUNNERS_WEBHOOK_SECRET_FILE` and `RUNNERS_RECONCILE_ACCOUNTS` (the scheduler's and the push subscriptions' accounts), as `cmd/runners` reads them; it listens on `PORT`, 8080.
