# The proxy's git machine, lsr-proxy-git (specs/0006-proxy.md, Git): a
# standard machine with a persistent disk for its mirrors, in the service's
# network, alone in its own subnetwork at that subnetwork's first address.
# The network's one firewall rule lets only the service's machines reach it,
# on port 443 (bootstrap/network.tf). Its external address is for its own
# fetches from upstream; no rule lets anything reach it there.
#
# It runs Container-Optimized OS, and the git server's image by digest. A new
# image replaces the machine, keeping its address and its mirrors disk.

data "google_compute_subnetwork" "proxy_git" {
  name   = "lanescape-runners-proxy-git"
  region = var.region
}

data "google_compute_image" "cos" {
  project = "cos-cloud"
  family  = "cos-stable"
}

locals {
  proxy_git_address = cidrhost(data.google_compute_subnetwork.proxy_git.ip_cidr_range, 2)

  # Mounts the mirrors disk. It makes a filesystem only on a disk that has no
  # signature blkid recognises and whose first 64 MiB are all zeros, as a new
  # disk's are, so a disk that has held mirrors is never formatted again,
  # even if its filesystem is damaged; that stops the server instead.
  proxy_git_mount = <<-EOT
    #!/bin/bash
    set -euo pipefail
    dev=/dev/disk/by-id/google-mirrors
    dir=/mnt/disks/mirrors
    for _ in $(seq 1 30); do
      [ -b "$dev" ] && break
      sleep 2
    done
    [ -b "$dev" ]
    rc=0
    blkid -p "$dev" >/dev/null 2>&1 || rc=$?
    if [ "$rc" -eq 2 ]; then
      # Under pipefail, a failed read fails the assignment, and so the script.
      if ! nonzero=$(head -c 67108864 "$dev" | tr -d '\000' | wc -c); then
        echo "couldn't read $dev; not formatting it" >&2
        exit 1
      fi
      if [ "$nonzero" -ne 0 ]; then
        echo "$dev has no filesystem blkid recognises, but isn't blank; not formatting it" >&2
        exit 1
      fi
      mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$dev"
    elif [ "$rc" -ne 0 ]; then
      echo "blkid failed with $rc on $dev" >&2
      exit 1
    fi
    mkdir -p "$dir"
    mountpoint -q "$dir" || mount -o discard,defaults "$dev" "$dir"
    chown 65532:65532 "$dir"
  EOT

  proxy_git_unit = <<-EOT
    [Unit]
    Description=The Lanescape Runners proxy's git server
    Wants=network-online.target
    After=network-online.target

    [Service]
    Environment=HOME=/home/proxy-git
    ExecStartPre=/bin/mkdir -p /home/proxy-git
    ExecStartPre=/bin/bash /etc/proxy-git/mount.sh
    ExecStartPre=/usr/bin/docker-credential-gcr configure-docker --registries=${var.region}-docker.pkg.dev
    ExecStartPre=-/usr/bin/docker rm -f proxy-git
    ExecStart=/usr/bin/docker run --rm --name proxy-git -p 443:8443 -v /mnt/disks/mirrors:/var/lib/proxy-git -e PORT=8443 -e RUNNERS_AUDIENCE=${local.proxy_url} -e RUNNERS_BUCKET=${google_storage_bucket.state.name} -e RUNNERS_MIRRORS=/var/lib/proxy-git ${var.proxy_git_image}
    ExecStop=/usr/bin/docker stop proxy-git
    Restart=always
    RestartSec=5
  EOT

  proxy_git_user_data = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/proxy-git/mount.sh
        permissions: "0755"
        owner: root
        encoding: b64
        content: ${base64encode(local.proxy_git_mount)}
      - path: /etc/systemd/system/proxy-git.service
        permissions: "0644"
        owner: root
        encoding: b64
        content: ${base64encode(local.proxy_git_unit)}
    runcmd:
      - systemctl daemon-reload
      - systemctl start proxy-git.service
  EOT
}

# What the machine boots with; a change replaces the machine.
resource "terraform_data" "proxy_git_boot" {
  input = local.proxy_git_user_data
}

resource "google_compute_disk" "proxy_git_mirrors" {
  name   = "lsr-proxy-git-mirrors"
  zone   = var.proxy_git_zone
  type   = "pd-balanced"
  size   = var.proxy_git_disk_gb
  labels = local.labels
}

resource "google_compute_instance" "proxy_git" {
  name         = "lsr-proxy-git"
  description  = "The Lanescape Runners proxy's git mirrors."
  zone         = var.proxy_git_zone
  machine_type = var.proxy_git_machine_type
  labels       = local.labels

  boot_disk {
    auto_delete = true

    initialize_params {
      image = data.google_compute_image.cos.self_link
      size  = 20
      type  = "pd-balanced"
    }
  }

  attached_disk {
    source      = google_compute_disk.proxy_git_mirrors.id
    device_name = "mirrors"
  }

  network_interface {
    subnetwork = data.google_compute_subnetwork.proxy_git.self_link
    network_ip = local.proxy_git_address

    # An ephemeral external address, for fetches from upstream only.
    access_config {}
  }

  service_account {
    email  = local.accounts.proxy_git
    scopes = ["cloud-platform"]
  }

  scheduling {
    provisioning_model  = "STANDARD"
    preemptible         = false
    automatic_restart   = true
    on_host_maintenance = "MIGRATE"
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    user-data                 = local.proxy_git_user_data
    block-project-ssh-keys    = "TRUE"
    enable-oslogin            = "TRUE"
    google-logging-enabled    = "false"
    google-monitoring-enabled = "false"
  }

  lifecycle {
    # A newer Container-Optimized OS is picked up when the machine is next
    # replaced, not by replacing it for that alone.
    ignore_changes       = [boot_disk[0].initialize_params[0].image]
    replace_triggered_by = [terraform_data.proxy_git_boot]
  }

  depends_on = [google_storage_bucket_iam_member.state_proxies]
}
