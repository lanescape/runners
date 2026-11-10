# The Terraform state bucket, which holds this configuration's own state.
# The maintainer makes it by hand before the first apply (README.md), and
# this adopts it; once it's in the state, the import does nothing.
import {
  to = google_storage_bucket.state
  id = "lanescape-runners-tfstate"
}

resource "google_storage_bucket" "state" {
  name                        = "lanescape-runners-tfstate"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels
  deletion_policy             = "PREVENT"

  versioning {
    enabled = true
  }

  depends_on = [google_project_service.apis]
}

# Where Infrastructure Manager keeps the deployment's artifacts and logs.
# CD names it with --artifacts-gcs-bucket, so Infrastructure Manager never
# needs to make a bucket of its own, and the infra identity needs no
# project-wide Cloud Storage access.
resource "google_storage_bucket" "infra_manager" {
  name                        = "lanescape-runners-infra-manager"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels

  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "infra_manager_infra" {
  for_each = toset(["roles/storage.objectAdmin", "roles/storage.legacyBucketReader"])

  bucket = google_storage_bucket.infra_manager.name
  role   = each.value
  member = google_service_account.infra.member
}

# So CD can print the preview's and the apply's logs.
resource "google_storage_bucket_iam_member" "infra_manager_deployer" {
  bucket = google_storage_bucket.infra_manager.name
  role   = "roles/storage.objectViewer"
  member = google_service_account.deployer.member
}
