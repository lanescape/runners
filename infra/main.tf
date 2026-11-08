# The service's own resources (specs/0004-deployment.md), applied only by
# Infrastructure Manager from CD, as lanescape-runners-infra, which may touch
# nothing else of the project's. The accounts, every project-level grant, the
# network, Pub/Sub, the scheduler job and the log sink are in bootstrap/.

locals {
  # Cloud Run's deterministic URLs. Each is also the audience its callers'
  # tokens name; bootstrap/ points the push subscriptions and the scheduler
  # job at the service's.
  service_url = "https://lanescape-runners-${data.google_project.this.number}.${var.region}.run.app"
  proxy_url   = "https://lanescape-runners-proxy-${data.google_project.this.number}.${var.region}.run.app"

  # The accounts bootstrap/ makes.
  accounts = {
    for k, id in {
      service   = "lanescape-runners-service"
      proxy     = "lanescape-runners-proxy"
      proxy_git = "lanescape-runners-proxy-git"
      scheduler = "lanescape-runners-scheduler"
      push      = "lanescape-runners-push"
    } : k => "${id}@${var.project_id}.iam.gserviceaccount.com"
  }

  labels = { lanescape-runners = "infra" }
}
