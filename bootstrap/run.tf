# The service on Cloud Run, as a shell that infra/ adopts and deploys
# (infra/run.tf). IAM can't limit Cloud Run by name, so it's made here, and
# the infra identity's grant is on the service itself: it can deploy this
# one, and reaches no other Cloud Run service in the project.
#
# The shell runs Google's sample container as the machines' account, which
# has no role, takes only internal traffic and keeps Cloud Run's invoker
# check, so nothing reaches it before the first deploy. After that, infra/
# owns everything about it, and this configuration changes nothing.

resource "google_cloud_run_v2_service" "shell" {
  name                = "lanescape-runners"
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
  name     = google_cloud_run_v2_service.shell.name
  location = google_cloud_run_v2_service.shell.location
  role     = google_project_iam_custom_role.infra_run.name
  member   = google_service_account.infra.member
}

# There were two shells, the service's and the proxy's, made with for_each.
# The service's keeps its place in the state under its new address. The
# proxy's, lanescape-runners-proxy, has deletion protection and ignores every
# change, so Terraform couldn't destroy it: it's forgotten, and the
# maintainer deletes it by hand (README.md, After removing the proxy). The
# infra identity's grant on it is destroyed with it. Once applied, these
# blocks can go.
moved {
  from = google_cloud_run_v2_service.shells["service"]
  to   = google_cloud_run_v2_service.shell
}

moved {
  from = google_cloud_run_v2_service_iam_member.infra["service"]
  to   = google_cloud_run_v2_service_iam_member.infra
}

removed {
  from = google_cloud_run_v2_service.shells

  lifecycle {
    destroy = false
  }
}
