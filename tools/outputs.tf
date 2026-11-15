output "bucket" {
  description = "The bucket's name."
  value       = google_storage_bucket.tools.name
}

output "workload_identity_provider" {
  description = "For google-github-actions/auth's workload_identity_provider."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "reader" {
  description = "The reader's email, for google-github-actions/auth's service_account."
  value       = google_service_account.reader.email
}
