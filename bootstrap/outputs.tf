output "service_url" {
  description = "The service's URL, and the audience its callers' tokens name."
  value       = local.service_url
}

output "accounts" {
  description = "The service's accounts."
  value = {
    service   = google_service_account.service.email
    machine   = google_service_account.machine.email
    scheduler = google_service_account.scheduler.email
    push      = google_service_account.push.email
    deployer  = google_service_account.deployer.email
    infra     = google_service_account.infra.email
  }
}

output "workload_identity_provider" {
  description = "For CD's google-github-actions/auth workload_identity_provider."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "image_repository" {
  description = "Where CD pushes the images (KO_DOCKER_REPO)."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "infra_manager_artifacts_bucket" {
  description = "For CD's --artifacts-gcs-bucket."
  value       = "gs://${google_storage_bucket.infra_manager.name}"
}

