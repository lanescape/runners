locals {
  apis = [
    "artifactregistry.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "cloudscheduler.googleapis.com",
    "compute.googleapis.com",
    "config.googleapis.com", # Infrastructure Manager
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "logging.googleapis.com",
    "pubsub.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
    "storage.googleapis.com",
    "sts.googleapis.com",
  ]
}

# Other teams' resources use these APIs too, so removing one from here, or
# destroying this configuration, leaves the API enabled.
resource "google_project_service" "apis" {
  for_each = toset(local.apis)

  service         = each.value
  deletion_policy = "ABANDON"
}
