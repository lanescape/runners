# The service on Cloud Run. It's reachable from the internet and takes every
# call without Cloud Run's own invoker check, since each of its endpoints
# authenticates its caller itself (specs/0001-architecture.md, Endpoints).
#
# bootstrap/ makes it as a shell, and grants the infra identity on it alone,
# since IAM can't limit Cloud Run by name; this adopts and deploys it. The
# infra identity can't make or delete a Cloud Run service.

import {
  to = google_cloud_run_v2_service.service
  id = "projects/lanescape-dev/locations/northamerica-northeast1/services/lanescape-runners"
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

    # A new revision with every apply (variables.tf).
    annotations = {
      "lanescape-runners/deploy" = var.deploy_id
    }

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
      # bootstrap/ makes the topic, and grants the service publishing on it.
      env {
        name  = "RUNNERS_WEBHOOK_TOPIC"
        value = "projects/${var.project_id}/topics/lanescape-runners-webhooks"
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
