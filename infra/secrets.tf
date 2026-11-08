# The App's secrets. Only their metadata is here; the maintainer adds each
# value by hand, once (README.md).

resource "google_secret_manager_secret" "app_key" {
  secret_id           = "lanescape-runners-app-key"
  labels              = local.labels
  deletion_protection = true

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }
}

resource "google_secret_manager_secret" "webhook_secret" {
  secret_id           = "lanescape-runners-webhook-secret"
  labels              = local.labels
  deletion_protection = true

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }
}

resource "google_secret_manager_secret_iam_member" "app_key" {
  for_each = toset([
    local.accounts.service,
    # The proxy asks GitHub about a caller's run, and reads a consumer's
    # .github/proxy.json, with the App (specs/0006-proxy.md).
    local.accounts.proxy,
  ])

  secret_id = google_secret_manager_secret.app_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${each.value}"
}

resource "google_secret_manager_secret_iam_member" "webhook_secret" {
  secret_id = google_secret_manager_secret.webhook_secret.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${local.accounts.service}"
}
