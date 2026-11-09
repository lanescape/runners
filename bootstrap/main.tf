locals {
  # Cloud Run's deterministic URLs, which infra/ uses too, so the push
  # subscriptions and the scheduler job can name the service before it
  # exists.
  service_url = "https://lanescape-runners-${data.google_project.this.number}.${var.region}.run.app"

  # The names infra/ gives the resources it manages. Its identity's grants
  # are limited to exactly these (roles.tf).
  infra_buckets = ["lanescape-runners", "lanescape-runners-cache"]
  infra_secrets = ["lanescape-runners-app-key", "lanescape-runners-webhook-secret"]

  labels = { lanescape-runners = "bootstrap" }
}
