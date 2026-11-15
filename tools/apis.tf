# The APIs the bucket and its sign-in need. bootstrap/ enables them too;
# both leave them enabled when removed or destroyed, so neither
# configuration can turn off an API the other, or another team, uses.
locals {
  apis = [
    "cloudresourcemanager.googleapis.com", # Terraform reads the project
    "iam.googleapis.com",
    "iamcredentials.googleapis.com", # the reader's tokens
    "storage.googleapis.com",
    "sts.googleapis.com", # GitHub's tokens, exchanged
  ]
}

resource "google_project_service" "apis" {
  for_each = toset(local.apis)

  service         = each.value
  deletion_policy = "ABANDON"
}
