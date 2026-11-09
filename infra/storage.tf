# The service's buckets (specs/0001-architecture.md, Components). Neither
# can be read from outside the project.

# The service's lease, records and boot logs. No object expiry applies: the
# service removes its own records.
resource "google_storage_bucket" "state" {
  name                        = "lanescape-runners"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels
}

resource "google_storage_bucket_iam_member" "state_service" {
  bucket = google_storage_bucket.state.name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${local.accounts.service}"
}

# The cache's uploads and entries (specs/0006-cache.md). The service is the
# only identity granted on it; a job reaches one object at a time, through
# a signed URL the service hands out, which acts as the service. Entries go
# as early as their rules allow, so the bucket is Standard class, since the
# others charge for early deletion, keeps no versions, and has soft delete
# off, which would bill every deleted entry for 7 more days.
resource "google_storage_bucket" "cache" {
  name                        = "lanescape-runners-cache"
  location                    = var.region
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels

  versioning {
    enabled = false
  }

  soft_delete_policy {
    retention_duration_seconds = 0
  }

  # The service deletes an upload that was never committed once its
  # reservation has expired; this is the backstop, for one it missed.
  lifecycle_rule {
    condition {
      age            = 1
      matches_prefix = ["uploads/"]
    }
    action {
      type = "Delete"
    }
  }
}

resource "google_storage_bucket_iam_member" "cache_service" {
  bucket = google_storage_bucket.cache.name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${local.accounts.service}"
}
