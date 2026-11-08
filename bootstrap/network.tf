# The service's network, lanescape-runners (specs/0002-machines.md, Network).
#
# It's here rather than in infra/ because IAM can't limit networks by name:
# an identity that could manage this network could add firewall rules to any
# network in the project. Here, nothing the infra identity holds can make,
# change or delete a network, a subnetwork or a firewall rule.
#
# It has exactly one firewall rule, below; check.sh refuses any other, here
# or in infra/. Otherwise the implicit rules apply: nothing reaches a
# machine, and each reaches out through its own external address.

resource "google_compute_network" "runners" {
  name                    = "lanescape-runners"
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

# Only the git machine, at its subnetwork's first address, so no machine can
# ever take its address.
resource "google_compute_subnetwork" "proxy_git" {
  name                     = "lanescape-runners-proxy-git"
  description              = "Only the proxy's git machine, lsr-proxy-git."
  network                  = google_compute_network.runners.id
  region                   = var.region
  ip_cidr_range            = var.proxy_git_cidr
  private_ip_google_access = true
}

# The service's machines reach the proxy's git mirror on port 443, matched by
# their accounts, which is how Google matches a rule's source account.
resource "google_compute_firewall" "proxy_git" {
  name        = "lanescape-runners-proxy-git"
  description = "The service's machines reach the proxy's git machine on 443. The network's only rule."
  network     = google_compute_network.runners.id
  direction   = "INGRESS"
  priority    = 1000

  source_service_accounts = [google_service_account.machine.email]
  target_service_accounts = [google_service_account.proxy_git.email]

  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
}

# The network attachment: the service puts its machines in their
# subnetwork, and the infra identity puts the git machine in its own; each
# on that subnetwork alone.
resource "google_compute_subnetwork_iam_member" "machines_service" {
  subnetwork = google_compute_subnetwork.machines.name
  region     = var.region
  role       = "roles/compute.networkUser"
  member     = google_service_account.service.member
}

resource "google_compute_subnetwork_iam_member" "proxy_git_infra" {
  subnetwork = google_compute_subnetwork.proxy_git.name
  region     = var.region
  role       = "roles/compute.networkUser"
  member     = google_service_account.infra.member
}
