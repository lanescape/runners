# Pub/Sub and the log sink (specs/0001-architecture.md, Components).
#
# The topics and subscriptions are here rather than in infra/ because IAM
# can't limit Pub/Sub by name: an identity that could manage these could
# read, redirect or delete every other topic and subscription in the
# project. Nothing in them changes with a deploy; the service's URL is
# Cloud Run's deterministic one.

# The webhook endpoint publishes each checked delivery here.
resource "google_pubsub_topic" "webhooks" {
  name   = "lanescape-runners-webhooks"
  labels = local.labels

  depends_on = [google_project_service.apis]
}

resource "google_pubsub_topic_iam_member" "webhooks_service" {
  topic  = google_pubsub_topic.webhooks.name
  role   = "roles/pubsub.publisher"
  member = google_service_account.service.member
}

# Compute Engine's preemption and guest-shutdown events for the service's
# machines, from the log sink below.
resource "google_pubsub_topic" "compute_events" {
  name   = "lanescape-runners-compute-events"
  labels = local.labels

  depends_on = [google_project_service.apis]
}

# The sink's filter is fixed here, so a merged change can't send the
# project's other logs to the service. It takes only the system events
# Compute Engine writes when a machine is preempted or its guest shuts down,
# and only for the service's job machines, lsr-<repository ID>-<random>, in
# its zones.
resource "google_logging_project_sink" "compute_events" {
  name                   = "lanescape-runners-compute-events"
  description            = "Preemption and guest-shutdown events of the Lanescape Runners machines, for the reconcile endpoint."
  destination            = "pubsub.googleapis.com/${google_pubsub_topic.compute_events.id}"
  unique_writer_identity = true

  filter = <<-EOT
    logName="projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Fsystem_event"
    resource.type="gce_instance"
    protoPayload.methodName=("compute.instances.preempted" OR "compute.instances.guestTerminate")
    protoPayload.resourceName=~"^projects/[^/]+/zones/(${join("|", var.zones)})/instances/lsr-[0-9]+-[a-z0-9-]+$"
  EOT
}

resource "google_pubsub_topic_iam_member" "compute_events_sink" {
  topic  = google_pubsub_topic.compute_events.name
  role   = "roles/pubsub.publisher"
  member = google_logging_project_sink.compute_events.writer_identity
}

# Both topics are pushed to the reconcile endpoint, signed as the push
# account, with acknowledgement deadlines of 10 minutes, above a pass's
# deadline (specs/0001-architecture.md, Passes). A push that finds the lease
# held gets a retryable error, and Pub/Sub delivers it again with backoff.
resource "google_pubsub_subscription" "reconcile" {
  for_each = {
    webhooks       = google_pubsub_topic.webhooks
    compute_events = google_pubsub_topic.compute_events
  }

  name                 = "${each.value.name}-reconcile"
  topic                = each.value.id
  ack_deadline_seconds = 600
  labels               = local.labels

  push_config {
    push_endpoint = "${local.service_url}/v1/reconcile"

    oidc_token {
      service_account_email = google_service_account.push.email
      audience              = local.service_url
    }
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "600s"
  }

  # Never expire for want of activity.
  expiration_policy {
    ttl = ""
  }

  depends_on = [google_service_account_iam_member.pubsub_agent_push]
}

# Pub/Sub's own service agent mints the push account's identity tokens; it may
# do that, and only that, for this account alone.
resource "google_service_account_iam_member" "pubsub_agent_push" {
  service_account_id = google_service_account.push.name
  role               = "roles/iam.serviceAccountOpenIdTokenCreator"
  member             = "serviceAccount:service-${data.google_project.this.number}@gcp-sa-pubsub.iam.gserviceaccount.com"

  depends_on = [google_project_service.apis]
}
