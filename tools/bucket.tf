# The organization's build tools (specs/0006-tools.md): files any of its
# repositories' builds may copy onto their machines, at
# <platform>/<tool>/<version>/<file>, each with <file>.sha256 beside it.
# Some may never be published or redistributed, such as Apple's macOS SDK,
# so the bucket is private and stays so: public access prevention is
# enforced, ACLs are off, and the only grant on it is the reader's (below).
# Only the maintainer uploads, with their own account; nothing here can
# write to it.
resource "google_storage_bucket" "tools" {
  name                        = "lanescape-ci-tools"
  location                    = var.region
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels

  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "reader" {
  bucket = google_storage_bucket.tools.name
  role   = "roles/storage.objectViewer"
  member = google_service_account.reader.member
}
