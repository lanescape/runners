# Every service account of the service. infra/ can make none, and holds no
# keys for any; nothing here makes a key.

resource "google_service_account" "service" {
  account_id   = "lanescape-runners-service"
  display_name = "Lanescape Runners service"
  description  = "The service on Cloud Run: makes and deletes the lsr- machines, disks and images (specs/0001-architecture.md)."
  depends_on   = [google_project_service.apis]
}

# Exists only because Compute Engine issues identity tokens only to machines
# with an account. It has no role on anything (specs/0002-machines.md).
resource "google_service_account" "machine" {
  account_id   = "lanescape-runners-machine"
  display_name = "Lanescape Runners machines"
  description  = "The account of the service's job machines. It has no role anywhere; never grant it one."
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "proxy" {
  account_id   = "lanescape-runners-proxy"
  display_name = "Lanescape Runners proxy"
  description  = "The HTTP proxy on Cloud Run: its own bucket, and what it needs to check callers (specs/0006-proxy.md)."
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "proxy_git" {
  account_id   = "lanescape-runners-proxy-git"
  display_name = "Lanescape Runners proxy git machine"
  description  = "The proxy's git machine, lsr-proxy-git: no more than checking callers (specs/0006-proxy.md)."
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "scheduler" {
  account_id   = "lanescape-runners-scheduler"
  display_name = "Lanescape Runners scheduler"
  description  = "Signs the scheduler's calls to the reconcile endpoint. It has no role anywhere."
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "push" {
  account_id   = "lanescape-runners-push"
  display_name = "Lanescape Runners pushes"
  description  = "Signs the push subscriptions' deliveries to the reconcile endpoint. It has no role anywhere."
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "deployer" {
  account_id   = "lanescape-runners-deployer"
  display_name = "Lanescape Runners CD"
  description  = "CD, signed in only from a push to main by cd.yml: pushes images and starts applies of infra/ (specs/0004-deployment.md)."
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "infra" {
  account_id   = "lanescape-runners-infra"
  display_name = "Lanescape Runners infrastructure"
  description  = "Infrastructure Manager applies infra/ as this account: the service's own resources, by name (specs/0004-deployment.md)."
  depends_on   = [google_project_service.apis]
}

# Who may act as whom. The service attaches only the machines' account to
# what it makes; the infra identity deploys as the service, the proxy and the
# git machine; CD starts applies as the infra identity.
resource "google_service_account_iam_member" "act_as" {
  for_each = {
    service_as_machine = { account = google_service_account.machine.name, member = google_service_account.service.member }
    infra_as_service   = { account = google_service_account.service.name, member = google_service_account.infra.member }
    infra_as_proxy     = { account = google_service_account.proxy.name, member = google_service_account.infra.member }
    infra_as_proxy_git = { account = google_service_account.proxy_git.name, member = google_service_account.infra.member }
    deployer_as_infra  = { account = google_service_account.infra.name, member = google_service_account.deployer.member }
  }

  service_account_id = each.value.account
  role               = "roles/iam.serviceAccountUser"
  member             = each.value.member
}
