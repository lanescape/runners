# The service's images, which CD builds with ko and pushes.
resource "google_artifact_registry_repository" "images" {
  repository_id = "lanescape-runners"
  location      = var.region
  format        = "DOCKER"
  description   = "Images of the Lanescape Runners service, pushed by CD."
  labels        = local.labels

  # Tags aren't immutable: Artifact Registry's cleanup never deletes a
  # tagged image in a repository whose tags are, and CD tags every image.
  # Each push's tag is its own anyway, of its commit, run and attempt, and
  # what's deployed is the digest alone (specs/0004-deployment.md).
  docker_config {
    immutable_tags = false
  }

  # Every deploy pushes an image, and Cloud Run keeps its own copy of what
  # it serves. The ten most recent are kept; every other is deleted. A
  # keep policy wins over a delete one, and Artifact Registry applies both
  # on its own schedule, so a few more may stay for a while.
  cleanup_policy_dry_run = false

  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"

    most_recent_versions {
      keep_count = 10
    }
  }

  cleanup_policies {
    id     = "delete-older"
    action = "DELETE"

    condition {
      tag_state = "ANY"
    }
  }

  depends_on = [google_project_service.apis]
}

resource "google_artifact_registry_repository_iam_member" "deployer" {
  repository = google_artifact_registry_repository.images.name
  location   = google_artifact_registry_repository.images.location
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.deployer.member
}

# The infra identity reads what it deploys. Cloud Run's own service agent
# reads the repository through its project role.
resource "google_artifact_registry_repository_iam_member" "readers" {
  for_each = {
    infra = google_service_account.infra.member
  }

  repository = google_artifact_registry_repository.images.name
  location   = google_artifact_registry_repository.images.location
  role       = "roles/artifactregistry.reader"
  member     = each.value
}
