# Boot logs (specs/0002-machines.md, A machine's life). Compute Engine sends
# each machine's serial console to Cloud Logging, which keeps a stopped or
# deleted machine's console where Compute Engine no longer serves it. A sink
# with a fixed filter routes only the serial console entries of the
# service's own machines, by their lsr- names, in its zones,
# to a log bucket of the service's own, and the service's account alone may
# read it, through a view. So the service reads its machines' consoles and
# no other machine's; a log view of the project's default bucket can't be
# limited by instance name, so the entries are routed rather than viewed in
# place.
resource "google_logging_project_bucket_config" "serial" {
  project        = var.project_id
  location       = "global"
  bucket_id      = "lanescape-runners-serial"
  description    = "Serial consoles of the Lanescape Runners machines, read by the service for boot logs."
  retention_days = 1
  depends_on     = [google_project_service.apis]
}

resource "google_logging_project_sink" "serial" {
  name        = "lanescape-runners-serial"
  description = "Serial consoles of the Lanescape Runners machines, for their boot logs."
  destination = "logging.googleapis.com/${google_logging_project_bucket_config.serial.id}"

  filter = <<-EOT
    logName="projects/${var.project_id}/logs/serialconsole.googleapis.com%2Fserial_port_1_output"
    resource.type="gce_instance"
    resource.labels.zone=(${join(" OR ", formatlist("\"%s\"", var.zones))})
    labels."compute.googleapis.com/resource_name"=~"^lsr-[0-9]+-[a-z0-9-]+$"
  EOT
}

resource "google_logging_log_view" "serial" {
  name        = "lanescape-runners-serial"
  bucket      = google_logging_project_bucket_config.serial.id
  description = "Everything in the service's serial console bucket, which the service's account alone may read."
}

resource "google_logging_log_view_iam_member" "serial_service" {
  parent   = "projects/${var.project_id}"
  location = google_logging_project_bucket_config.serial.location
  bucket   = google_logging_project_bucket_config.serial.bucket_id
  name     = google_logging_log_view.serial.name
  role     = "roles/logging.viewAccessor"
  member   = google_service_account.service.member
}
