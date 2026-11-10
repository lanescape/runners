# The service's network, lanescape-runners (specs/0002-machines.md, Network).
#
# It's here rather than in infra/ because IAM can't limit networks by name:
# an identity that could manage this network could add firewall rules to any
# network in the project. Here, nothing the infra identity holds can make,
# change or delete a network, a subnetwork or a firewall rule.
#
# It has no firewall rules; check.sh refuses any, here or in infra/. Its
# implicit rules apply: nothing reaches a machine, and each reaches out
# through its own external address.

resource "google_compute_network" "runners" {
  name = "lanescape-runners"
  # A network's description is fixed when it's made: changing it would
  # replace the network, and every subnetwork in it. It still names the
  # proxy's git machine, which is gone.
  description             = "The Lanescape Runners machines and the proxy's git machine. One firewall rule; see bootstrap/network.tf."
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"

  depends_on = [google_project_service.apis]
}

resource "google_compute_subnetwork" "machines" {
  name                     = "lanescape-runners"
  description              = "The service's job machines and base image builders."
  network                  = google_compute_network.runners.id
  region                   = var.region
  ip_cidr_range            = var.machines_cidr
  private_ip_google_access = true
}

# The network attachment: the service puts its machines in their
# subnetwork, and only there.
resource "google_compute_subnetwork_iam_member" "machines_service" {
  subnetwork = google_compute_subnetwork.machines.name
  region     = var.region
  role       = "roles/compute.networkUser"
  member     = google_service_account.service.member
}
