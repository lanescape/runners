# The service's buckets (specs/0001-architecture.md, Components). Neither
# can be read from outside the project. No object expiry applies: what the
# specs expire are images, kept by the service, and git mirrors, on the git
# machine's disk; the service removes its own records.

resource "google_storage_bucket" "state" {
  name                        = "lanescape-runners"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels
}

# The service's lease, records and boot logs.
resource "google_storage_bucket_iam_member" "state_service" {
  bucket = google_storage_bucket.state.name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${local.accounts.service}"
}

# The proxy and the git machine read the service's records to check that a
# caller's machine has an admitted job running now (specs/0006-proxy.md).
resource "google_storage_bucket_iam_member" "state_proxies" {
  for_each = toset([local.accounts.proxy, local.accounts.proxy_git])

  bucket = google_storage_bucket.state.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${each.value}"
}

# The proxy's stored responses, and its temporary objects while a response
# arrives.
resource "google_storage_bucket" "proxy" {
  name                        = "lanescape-runners-proxy"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  labels                      = local.labels
}

resource "google_storage_bucket_iam_member" "proxy_proxy" {
  bucket = google_storage_bucket.proxy.name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${local.accounts.proxy}"
}
