locals {
  # Cloud Run's deterministic URLs, which infra/ uses too, so the push
  # subscriptions and the scheduler job can name the service before it
  # exists.
  service_url = "https://lanescape-runners-${data.google_project.this.number}.${var.region}.run.app"

  # The names infra/ gives the resources it manages. Its identity's grants
  # are limited to exactly these (roles.tf).
  infra_buckets = ["lanescape-runners", "lanescape-runners-proxy"]
  infra_secrets = ["lanescape-runners-app-key", "lanescape-runners-webhook-secret"]
  proxy_git     = "lsr-proxy-git"
  proxy_git_disks = [
    "lsr-proxy-git",         # its boot disk
    "lsr-proxy-git-mirrors", # its mirrors
  ]

  labels = { lanescape-runners = "bootstrap" }
}
