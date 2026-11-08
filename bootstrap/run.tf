# The two Cloud Run services, as shells that infra/ adopts and deploys
# (infra/run.tf). IAM can't limit Cloud Run by name, so they're made here,
# and the infra identity's grant is on each service itself: it can deploy
# these two, and reaches no other Cloud Run service in the project.
#
# A shell runs Google's sample container as the machines' account, which has
# no role, takes only internal traffic and keeps Cloud Run's invoker check,
# so nothing reaches it before the first deploy. After that, infra/ owns
# everything about it, and this configuration changes nothing.
locals {
  run_services = {
    service = "lanescape-runners"
    proxy   = "lanescape-runners-proxy"
  }
}

resource "google_cloud_run_v2_service" "shells" {
  for_each = local.run_services

  name                = each.value
  location            = var.region
  ingress             = "INGRESS_TRAFFIC_INTERNAL_ONLY"
  deletion_protection = true

  template {
    service_account = google_service_account.machine.email

    containers {
      image = "us-docker.pkg.dev/cloudrun/container/hello"
    }
  }

  lifecycle {
    ignore_changes = all
  }

  depends_on = [google_project_service.apis]
}

resource "google_cloud_run_v2_service_iam_member" "infra" {
  for_each = google_cloud_run_v2_service.shells

  name     = each.value.name
  location = each.value.location
  role     = google_project_iam_custom_role.infra_run.name
  member   = google_service_account.infra.member
}
