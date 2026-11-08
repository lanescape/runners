# The service's and the proxy's images, which CD builds with ko and pushes.
resource "google_artifact_registry_repository" "images" {
  repository_id = "lanescape-runners"
  location      = var.region
  format        = "DOCKER"
  description   = "Images of the Lanescape Runners service and its proxy, pushed by CD."
  labels        = local.labels

  docker_config {
    immutable_tags = true
  }

  depends_on = [google_project_service.apis]
}

resource "google_artifact_registry_repository_iam_member" "deployer" {
  repository = google_artifact_registry_repository.images.name
  location   = google_artifact_registry_repository.images.location
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.deployer.member
}

# The git machine pulls its own image; the infra identity reads what it
# deploys. Cloud Run's own service agent reads the repository through its
# project role.
resource "google_artifact_registry_repository_iam_member" "readers" {
  for_each = {
    proxy_git = google_service_account.proxy_git.member
    infra     = google_service_account.infra.member
  }

  repository = google_artifact_registry_repository.images.name
  location   = google_artifact_registry_repository.images.location
  role       = "roles/artifactregistry.reader"
  member     = each.value
}
