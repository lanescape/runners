# Who reads the bucket (specs/0006-tools.md, Reading): a job of any of the
# organization's repositories, by the organization's ID, signed in with
# GitHub's token through this pool as lanescape-ci-tools-reader, except a
# pull_request_target run's.

resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "lanescape-ci-tools"
  display_name              = "Lanescape CI tools"
  description               = "GitHub Actions: the organization's repositories, by its ID, except pull_request_target runs. Reads lanescape-ci-tools."

  depends_on = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "GitHub Actions"
  description                        = "GitHub's OIDC tokens for the organization's repositories, except pull_request_target runs."

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }

  # google.subject may be at most 127 bytes, which a token's sub, naming a
  # repository and a branch of any of the organization's, can exceed. The
  # run, by its repository's ID, is short and names what signed in.
  attribute_mapping = {
    "google.subject"                = "assertion.repository_id + \"/\" + assertion.run_id + \"/\" + assertion.run_attempt"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
  }

  # GitHub signs every claim used here, and sets repository_owner_id to the
  # ID of the owner of the repository the run is in, which no workflow can
  # choose: a fork's own runs carry its owner's. A fork's pull_request run
  # into the organization gets no token, so long as the organization never
  # sends write tokens to fork pull requests' workflows in its private
  # repositories, which the maintainer keeps off (README.md). A
  # pull_request_target run does get one, of the organization's repository,
  # while its jobs may run a fork's code, so it's refused.
  attribute_condition = join(" && ", [
    "assertion.repository_owner_id == \"${var.github_owner_id}\"",
    "assertion.event_name != \"pull_request_target\"",
  ])
}

resource "google_service_account" "reader" {
  account_id   = "lanescape-ci-tools-reader"
  display_name = "Lanescape CI tools reader"
  description  = "Reads lanescape-ci-tools, and nothing else, for the organization's GitHub Actions jobs (specs/0006-tools.md)."

  depends_on = [google_project_service.apis]
}

resource "google_service_account_iam_member" "reader_workload_identity" {
  service_account_id = google_service_account.reader.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository_owner_id/${var.github_owner_id}"
}
