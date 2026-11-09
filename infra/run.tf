# The service and the HTTP proxy on Cloud Run. Both are reachable from the
# internet and take every call without Cloud Run's own invoker check, since
# each of their endpoints authenticates its caller itself
# (specs/0001-architecture.md, Endpoints; specs/0006-proxy.md).
#
# bootstrap/ makes each as a shell, and grants the infra identity on it
# alone, since IAM can't limit Cloud Run by name; this adopts and deploys
# them. The infra identity can't make or delete a Cloud Run service.

import {
  to = google_cloud_run_v2_service.service
  id = "projects/lanescape-dev/locations/northamerica-northeast1/services/lanescape-runners"
}

import {
  to = google_cloud_run_v2_service.proxy
  id = "projects/lanescape-dev/locations/northamerica-northeast1/services/lanescape-runners-proxy"
}

resource "google_cloud_run_v2_service" "service" {
  name                 = "lanescape-runners"
  description          = "Lanescape Runners: GitHub Actions runners for the lanescape organization."
  location             = var.region
  ingress              = "INGRESS_TRAFFIC_ALL"
  invoker_iam_disabled = true
  deletion_protection  = true
  labels               = local.labels

  template {
    service_account = local.accounts.service

    # Above a pass's 7-minute deadline, as the scheduler's attempt deadline
    # and the subscriptions' acknowledgement deadlines are
    # (specs/0001-architecture.md, Passes).
    timeout = "600s"

    # One request at a time per instance, so a pass has its instance to
    # itself; a webhook or a machine's check meanwhile starts another.
    max_instance_request_concurrency = 1

    scaling {
      min_instance_count = 0
      max_instance_count = 20
    }

    containers {
      image = var.service_image

      ports {
        container_port = 8080
      }

      # CPU stays allocated outside requests: a supervisor's report starts
      # a pass after its response has been sent.
      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle = false
      }

      # The service refuses to start unless this is endpoints.Service.
      env {
        name  = "RUNNERS_AUDIENCE"
        value = local.service_url
      }
      env {
        name  = "RUNNERS_PROJECT"
        value = var.project_id
      }
      env {
        name  = "RUNNERS_BUCKET"
        value = google_storage_bucket.state.name
      }
      env {
        name  = "RUNNERS_GITHUB_APP_ID"
        value = tostring(var.github_app_id)
      }
      env {
        name  = "RUNNERS_GITHUB_APP_KEY_FILE"
        value = "/secrets/app-key/key.pem"
      }
      env {
        name  = "RUNNERS_WEBHOOK_SECRET_FILE"
        value = "/secrets/webhook-secret/secret"
      }
      env {
        name  = "RUNNERS_RECONCILE_ACCOUNTS"
        value = join(",", [local.accounts.scheduler, local.accounts.push])
      }

      volume_mounts {
        name       = "app-key"
        mount_path = "/secrets/app-key"
      }
      volume_mounts {
        name       = "webhook-secret"
        mount_path = "/secrets/webhook-secret"
      }
    }

    volumes {
      name = "app-key"
      secret {
        secret = google_secret_manager_secret.app_key.secret_id
        items {
          version = "latest"
          path    = "key.pem"
        }
      }
    }
    volumes {
      name = "webhook-secret"
      secret {
        secret = google_secret_manager_secret.webhook_secret.secret_id
        items {
          version = "latest"
          path    = "secret"
        }
      }
    }
  }

  depends_on = [
    google_storage_bucket_iam_member.state_service,
    google_secret_manager_secret_iam_member.app_key,
    google_secret_manager_secret_iam_member.webhook_secret,
  ]
}

resource "google_cloud_run_v2_service" "proxy" {
  name                 = "lanescape-runners-proxy"
  description          = "The Lanescape Runners proxy: cached reads of HTTP upstreams."
  location             = var.region
  ingress              = "INGRESS_TRAFFIC_ALL"
  invoker_iam_disabled = true
  deletion_protection  = true
  labels               = local.labels

  template {
    service_account = local.accounts.proxy

    # Responses are streamed, and can be large.
    timeout = "3600s"

    scaling {
      min_instance_count = 0
      max_instance_count = 10
    }

    containers {
      image = var.proxy_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle = true
      }

      env {
        name  = "RUNNERS_AUDIENCE"
        value = local.proxy_url
      }
      env {
        name  = "RUNNERS_PROXY_BUCKET"
        value = google_storage_bucket.proxy.name
      }
      env {
        name  = "RUNNERS_BUCKET"
        value = google_storage_bucket.state.name
      }
      env {
        name  = "RUNNERS_GITHUB_APP_ID"
        value = tostring(var.github_app_id)
      }
      env {
        name  = "RUNNERS_GITHUB_APP_KEY_FILE"
        value = "/secrets/app-key/key.pem"
      }

      volume_mounts {
        name       = "app-key"
        mount_path = "/secrets/app-key"
      }
    }

    volumes {
      name = "app-key"
      secret {
        secret = google_secret_manager_secret.app_key.secret_id
        items {
          version = "latest"
          path    = "key.pem"
        }
      }
    }
  }

  depends_on = [
    google_storage_bucket_iam_member.state_proxies,
    google_storage_bucket_iam_member.proxy_proxy,
    google_secret_manager_secret_iam_member.app_key,
  ]
}
