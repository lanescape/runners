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
