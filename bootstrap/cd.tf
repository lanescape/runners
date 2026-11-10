# CD's sign-in (specs/0004-deployment.md, CI and CD): only a push to main,
# by cd.yml, in this repository, checked by the repository's and the
# organization's IDs, may act as lanescape-runners-deployer.

resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "lanescape-runners-github"
  display_name              = "Lanescape Runners CD"
  description               = "GitHub Actions: only a push to main by cd.yml in lanescape/runners."

  depends_on = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "GitHub Actions"
  description                        = "GitHub's OIDC tokens for a push to main by cd.yml in lanescape/runners."

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }

  attribute_mapping = {
    "google.subject"                = "assertion.sub"
    "attribute.repository_id"       = "assertion.repository_id"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
    "attribute.workflow_ref"        = "assertion.workflow_ref"
  }

  # GitHub signs every claim used here. The workflow is named relative to
  # the repository the token names, whose ID is checked, so a rename keeps
  # working and nothing in another repository matches. A job that calls a
  # reusable workflow has that workflow's job_workflow_ref, which isn't
  # cd.yml's, so only cd.yml's own jobs sign in. GitHub sets job_workflow_ref
  # on every job, to workflow_ref for one that isn't reusable; a token
  # without it is still only one of cd.yml's own jobs.
  attribute_condition = join(" && ", [
    "assertion.repository_id == \"${var.github_repository_id}\"",
    "assertion.repository_owner_id == \"${var.github_owner_id}\"",
    "assertion.event_name == \"push\"",
    "assertion.ref == \"refs/heads/main\"",
    "assertion.ref_type == \"branch\"",
    "assertion.workflow_ref == assertion.repository + \"/.github/workflows/cd.yml@refs/heads/main\"",
    "(!has(assertion.job_workflow_ref) || assertion.job_workflow_ref == assertion.workflow_ref)",
  ])
}

resource "google_service_account_iam_member" "deployer_workload_identity" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository_id/${var.github_repository_id}"
}

