#!/bin/bash
# CD's preview and apply of infra/ (specs/0004-deployment.md, CI and CD),
# which cd.yml's deploy job runs once it has pushed the service's image.
# It asks Infrastructure Manager for a preview, against the deployment if
# there is one yet, then applies, through Infrastructure Manager's API, and
# prints both one's logs from the artifacts bucket. It fails if anything
# does: the preview, the apply, a deployment that isn't ACTIVE or a latest
# revision that isn't APPLIED, or logs that can't be read. apply_test.go
# runs it against stand-ins for gcloud and curl.
#
# It takes, from the environment: PROJECT, REGION, DEPLOYMENT,
# INFRA_ACCOUNT, SOURCE, TF_VERSION, INFRA_MANAGER_BUCKET, GITHUB_APP_ID,
# IMAGE, and GitHub's GITHUB_SHA, GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT and
# GITHUB_STEP_SUMMARY.
set -euo pipefail
: "${PROJECT:?}" "${REGION:?}" "${DEPLOYMENT:?}" "${INFRA_ACCOUNT:?}" "${SOURCE:?}" "${TF_VERSION:?}" "${INFRA_MANAGER_BUCKET:?}" "${GITHUB_APP_ID:?}" "${IMAGE:?}"
deploy_id="$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
inputs="github_app_id=$GITHUB_APP_ID,service_image=$IMAGE,deploy_id=$deploy_id"
source_flags=(--service-account="$INFRA_ACCOUNT" --git-source-repo="$SOURCE" --git-source-directory=infra
  --git-source-ref="$GITHUB_SHA" --artifacts-gcs-bucket="$INFRA_MANAGER_BUCKET" --tf-version-constraint="$TF_VERSION"
  --input-values="$inputs")
logs_failed=false

# Prints what Infrastructure Manager logged, from its artifacts
# bucket, which the deployer may read. A location with nothing in
# it says so; one that can't be read fails the job, at its end.
print_logs() {
  local what=$1 where=$2 listing objects
  if [ -z "$where" ]; then
    echo "No logs recorded for the $what."
    return 0
  fi
  if ! listing=$(gcloud storage ls -r "$where" 2>&1); then
    if grep -q "matched no objects" <<<"$listing"; then
      echo "No logs found for the $what at $where."
      return 0
    fi
    echo "::error::Couldn't list the $what's logs at $where: $listing"
    logs_failed=true
    return 0
  fi
  # Terraform's plans and state are binary, and say nothing the
  # logs don't.
  objects=$(grep -v -E '/$|:$|\.(tfplan|tfstate|zip|gz|tar)$' <<<"$listing" || true)
  if [ -z "$objects" ]; then
    echo "No logs found for the $what at $where."
    return 0
  fi
  while read -r obj; do
    echo "::group::The $what's log, $obj"
    if ! gcloud storage cat "$obj"; then
      echo "::error::Couldn't read $obj"
      logs_failed=true
    fi
    echo "::endgroup::"
  done <<<"$objects"
}

# The preview, against the deployment if there is one yet.
exists=false
if out=$(gcloud infra-manager deployments describe "$DEPLOYMENT" --format='value(name)' 2>&1); then
  exists=true
elif ! grep -q NOT_FOUND <<<"$out"; then
  echo "$out"
  exit 1
fi
deployment_flags=()
if [ "$exists" = true ]; then
  deployment_flags=(--deployment="$DEPLOYMENT")
fi
preview="projects/$PROJECT/locations/$REGION/previews/lanescape-runners-$deploy_id"
trap 'gcloud infra-manager previews delete "$preview" --quiet >/dev/null 2>&1 || true' EXIT
preview_ok=true
gcloud infra-manager previews create "$preview" "${deployment_flags[@]}" "${source_flags[@]}" || preview_ok=false
gcloud infra-manager previews describe "$preview" --format='yaml(state,errorCode,tfErrors,errorLogs)' || logs_failed=true
if where=$(gcloud infra-manager previews describe "$preview" --format='value(logs)'); then
  print_logs preview "$where"
else
  echo "::error::Couldn't find the preview's logs."
  logs_failed=true
fi
if [ "$preview_ok" != true ] || [ "$(gcloud infra-manager previews describe "$preview" --format='value(state)')" != SUCCEEDED ]; then
  echo "::error title=Preview failed::Infrastructure Manager's preview of infra/ failed; nothing was applied."
  exit 1
fi

# The apply, which deploys the service with the infrastructure,
# through Infrastructure Manager's API itself: gcloud's apply of
# an existing deployment first lists the project's shared default
# staging bucket, which CD has no grant on and git sources never
# use. The access token goes only in a header, never to the log.
api=https://config.googleapis.com/v1
body=$(jq -n --arg sa "$INFRA_ACCOUNT" --arg bucket "$INFRA_MANAGER_BUCKET" --arg tf "$TF_VERSION" \
  --arg repo "$SOURCE" --arg ref "$GITHUB_SHA" --arg app "$GITHUB_APP_ID" --arg image "$IMAGE" --arg deploy "$deploy_id" '{
    serviceAccount: $sa, artifactsGcsBucket: $bucket, tfVersionConstraint: $tf,
    terraformBlueprint: {
      gitSource: {repo: $repo, directory: "infra", ref: $ref},
      inputValues: {github_app_id: {inputValue: $app}, service_image: {inputValue: $image}, deploy_id: {inputValue: $deploy}}
    }
  }')
# The token reaches curl as its configuration, on its standard
# input, so it's in no process's arguments either.
call() {
  local method=$1 url=$2 data=${3:-} token
  token=$(gcloud auth print-access-token)
  local args=(-sS --fail-with-body -X "$method" -K - -H "Content-Type: application/json")
  if [ -n "$data" ]; then
    args+=(--data "$data")
  fi
  printf 'header = "Authorization: Bearer %s"\n' "$token" | curl "${args[@]}" "$url"
}
apply_ok=true
if [ "$exists" = true ]; then
  op=$(call PATCH "$api/$DEPLOYMENT" "$body") || apply_ok=false
else
  op=$(call POST "$api/projects/$PROJECT/locations/$REGION/deployments?deploymentId=${DEPLOYMENT##*/}" "$body") || apply_ok=false
fi
if [ "$apply_ok" = true ]; then
  name=$(jq -r .name <<<"$op")
  echo "Applying: $name"
  until jq -e .done <<<"$op" >/dev/null; do
    sleep 15
    op=$(call GET "$api/$name") || { apply_ok=false; break; }
  done
  if [ "$apply_ok" = true ] && jq -e .error <<<"$op" >/dev/null; then
    jq .error <<<"$op"
    apply_ok=false
  fi
else
  echo "$op"
fi
if ! revision=$(gcloud infra-manager deployments describe "$DEPLOYMENT" --format='value(latestRevision)'); then
  echo "::error::Couldn't find the apply's revision."
  logs_failed=true
  revision=
fi
# The operation's end isn't the apply's success: the revision must
# be applied, and the deployment active.
if [ "$apply_ok" = true ]; then
  state=$(gcloud infra-manager deployments describe "$DEPLOYMENT" --format='value(state)' || true)
  revision_state=
  if [ -n "$revision" ]; then
    revision_state=$(gcloud infra-manager revisions describe "$revision" --format='value(state)' || true)
  fi
  if [ "$state" != ACTIVE ] || [ "$revision_state" != APPLIED ]; then
    echo "::error::The deployment is ${state:-unknown} and its latest revision ${revision_state:-unknown}, not ACTIVE and APPLIED."
    apply_ok=false
  fi
fi
if [ -n "$revision" ]; then
  gcloud infra-manager revisions describe "$revision" --format='yaml(name,state,stateDetail,errorCode,tfErrors,errorLogs)' || logs_failed=true
  if where=$(gcloud infra-manager revisions describe "$revision" --format='value(logs)'); then
    print_logs apply "$where"
  else
    echo "::error::Couldn't find the apply's logs."
    logs_failed=true
  fi
  echo "Revision: \`$revision\`" >>"$GITHUB_STEP_SUMMARY"
elif [ "$logs_failed" != true ]; then
  echo "::error::The deployment has no revision."
  logs_failed=true
fi
if [ "$apply_ok" != true ]; then
  echo "::error title=Apply failed::Infrastructure Manager's apply of infra/ failed: see its logs above."
  exit 1
fi
if [ "$logs_failed" = true ]; then
  echo "::error title=Logs unread::infra/ was applied, but its logs couldn't all be read."
  exit 1
fi
