# The service's bucket (specs/0001-architecture.md, Components), which can't
# be read from outside the project: its lease, records and boot logs. No
# object expiry applies: the service removes its own records.
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

# The caches' bucket (specs/0003-disks.md, Caches): every version of every
# consumer's caches, as its jobs saved them, under <repository ID>/<version>/.
# Jobs read and write it only with tokens the service downscopes from its own
# to one version's prefix, read-only or create-only. No object expiry or soft
# delete applies: the service deletes versions by its own rules, and a
# deleted version is never restored.
resource "google_storage_bucket" "caches" {
  name                        = "lanescape-runners-caches"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels

  soft_delete_policy {
    retention_duration_seconds = 0
  }
}

# Admin over the objects alone: what a downscoped token is narrowed from.
resource "google_storage_bucket_iam_member" "caches_service" {
  bucket = google_storage_bucket.caches.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${local.accounts.service}"
}
