# Calls the reconcile endpoint every 5 minutes, so a pass runs even when
# events are lost (specs/0001-architecture.md, Components). It's here rather
# than in infra/ because IAM can't limit Cloud Scheduler by name.
resource "google_cloud_scheduler_job" "reconcile" {
  name             = "lanescape-runners-reconcile"
  description      = "Runs a Lanescape Runners pass."
  region           = var.region
  schedule         = "*/5 * * * *"
  time_zone        = "Etc/UTC"
  attempt_deadline = "600s"

  # No retry_config: the default is no retries, and the next call is in 5
  # minutes anyway. Google doesn't store a retry count of zero, so a
  # retry_config saying so would show as a change in every plan.

  http_target {
    http_method = "POST"
    uri         = "${local.service_url}/v1/reconcile"

    oidc_token {
      service_account_email = google_service_account.scheduler.email
      audience              = local.service_url
    }
  }

  depends_on = [google_project_service.apis]
}
