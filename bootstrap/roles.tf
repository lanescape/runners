# The service's custom roles and every project-level grant.
#
# The project holds other teams' resources, so each grant is limited by IAM
# Conditions to the service's own resources by name wherever IAM can do
# that: Compute Engine instances, disks and images, Cloud Storage buckets and
# Secret Manager secrets. The Cloud Run service's grant is on the service
# itself (run.tf). Listing, operations, a few other reads and
# Infrastructure Manager can't be limited by name; those grants are apart
# and say so (README.md, What IAM can't limit). Every grant here adds a
# member; none replaces a role's or the project's policy.
#
# IAM allows at most 12 logical operators in one condition, so each
# resource type has a binding of its own.

locals {
  # A Compute Engine resource's zone, from its full name,
  # projects/<project>/zones/<zone>/instances/<name>.
  in_machine_zones = "(${join(" || ", [for z in var.zones : "resource.name.extract(\"/zones/{zone}/\") == \"${z}\""])})"

  # The service's own instances, disks and images: lsr-.
  service_compute = {
    instances = <<-EOT
      resource.type == "compute.googleapis.com/Instance" &&
      ${local.in_machine_zones} &&
      resource.name.extract("/instances/{name}").startsWith("lsr-")
    EOT
    disks     = <<-EOT
      resource.type == "compute.googleapis.com/Disk" &&
      ${local.in_machine_zones} &&
      resource.name.extract("/disks/{name}").startsWith("lsr-")
    EOT
    images    = <<-EOT
      resource.type == "compute.googleapis.com/Image" &&
      resource.name.extract("/images/{name}").startsWith("lsr-")
    EOT
  }

  # Exactly the resources infra/ manages that IAM can limit by name.
  infra_named = {
    buckets = <<-EOT
      resource.type == "storage.googleapis.com/Bucket" && (
        ${join(" ||\n  ", [for b in local.infra_buckets : "resource.name == \"projects/_/buckets/${b}\""])}
      )
    EOT
    secrets = <<-EOT
      resource.type == "secretmanager.googleapis.com/Secret" && (
        ${join(" ||\n  ", [for x in local.infra_secrets : "resource.name == \"projects/${data.google_project.this.number}/secrets/${x}\""])}
      )
    EOT
  }
}

# --- The service ------------------------------------------------------------

resource "google_project_iam_custom_role" "machines" {
  role_id     = "lanescapeRunnersMachines"
  title       = "Lanescape Runners machines"
  description = "Makes, uses and deletes the service's machines, disks and images. Granted only on lsr- names."
  permissions = [
    "compute.disks.create",
    "compute.disks.delete",
    "compute.disks.get",
    "compute.disks.resize",
    "compute.disks.setLabels",
    "compute.disks.use",
    "compute.disks.useReadOnly",
    "compute.images.create",
    "compute.images.delete",
    "compute.images.deprecate",
    "compute.images.get",
    "compute.images.setLabels",
    "compute.images.useReadOnly",
    "compute.instances.attachDisk",
    "compute.instances.create",
    "compute.instances.delete",
    "compute.instances.detachDisk",
    "compute.instances.get",
    "compute.instances.getGuestAttributes",
    "compute.instances.getSerialPortOutput",
    "compute.instances.setDiskAutoDelete",
    "compute.instances.setLabels",
    "compute.instances.setMetadata",
    "compute.instances.setScheduling",
    "compute.instances.setServiceAccount",
    "compute.instances.stop",
  ]
  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "service_machines" {
  for_each = local.service_compute

  project = var.project_id
  role    = google_project_iam_custom_role.machines.name
  member  = google_service_account.service.member

  condition {
    title       = "lanescape-runners-${each.key}"
    description = "Only the service's own lsr- ${each.key}."
    expression  = each.value
  }
}

# Lists and operations, which IAM can't limit by name.
resource "google_project_iam_custom_role" "compute_reader" {
  role_id     = "lanescapeRunnersComputeReader"
  title       = "Lanescape Runners compute reader"
  description = "Lists instances, disks and images, and reads zones, machine types and operations: what can't be limited by name."
  permissions = [
    "compute.disks.list",
    "compute.globalOperations.get",
    "compute.globalOperations.list",
    "compute.images.getFromFamily",
    "compute.images.list",
    "compute.instances.list",
    "compute.machineTypes.get",
    "compute.machineTypes.list",
    "compute.regions.get",
    "compute.zoneOperations.get",
    "compute.zoneOperations.list",
    "compute.zones.get",
    "compute.zones.list",
  ]
  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "service_compute_reader" {
  project = var.project_id
  role    = google_project_iam_custom_role.compute_reader.name
  member  = google_service_account.service.member
}

# The cache hands out V4 signed URLs for one object at a time
# (specs/0006-cache.md). The service has no key, so it signs with IAM's
# signBlob, as its own account and only that: this role holds nothing else,
# and is granted on the service's account alone.
resource "google_project_iam_custom_role" "signer" {
  role_id     = "lanescapeRunnersSigner"
  title       = "Lanescape Runners signer"
  description = "Signs blobs as an account. Granted only to the service, on its own account, for the cache's signed URLs."
  permissions = ["iam.serviceAccounts.signBlob"]
  depends_on  = [google_project_service.apis]
}

resource "google_service_account_iam_member" "service_signs_as_itself" {
  service_account_id = google_service_account.service.name
  role               = google_project_iam_custom_role.signer.name
  member             = google_service_account.service.member
}

# --- The infra identity -----------------------------------------------------
#
# It holds no project-level IAM administration, no service account creation
# or keys, no role definitions and no log sinks, so a merged change can't
# widen what any account holds in the project (specs/0004-deployment.md).

# Exactly the buckets and secrets infra/ manages, and the access to each,
# set on the resource itself.
resource "google_project_iam_custom_role" "infra_named" {
  role_id     = "lanescapeRunnersInfraNamed"
  title       = "Lanescape Runners infrastructure, by name"
  description = "Manages the service's buckets and secrets' metadata. Granted only on their names."
  permissions = [
    "secretmanager.secrets.delete",
    "secretmanager.secrets.get",
    "secretmanager.secrets.getIamPolicy",
    "secretmanager.secrets.setIamPolicy",
    "secretmanager.secrets.update",
    "storage.buckets.delete",
    "storage.buckets.get",
    "storage.buckets.getIamPolicy",
    "storage.buckets.setIamPolicy",
    "storage.buckets.update",
  ]
  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "infra_named" {
  for_each = local.infra_named

  project = var.project_id
  role    = google_project_iam_custom_role.infra_named.name
  member  = google_service_account.infra.member

  condition {
    title       = "lanescape-runners-infra-${each.key}"
    description = "Only the ${each.key} that infra/ manages, by name."
    expression  = each.value
  }
}

# What can't be limited by name: making a bucket or a secret (which touches
# nothing that exists), reading the project and Cloud Run's operations
# (Terraform polls its own; the identity can't list operations or read logs,
# so it can't learn another team's operation's name), writing Infrastructure
# Manager's logs, and the state permissions of Infrastructure Manager's own
# agent role, without its project-wide Cloud Storage and Cloud Build access.
resource "google_project_iam_custom_role" "infra_project" {
  role_id     = "lanescapeRunnersInfraProject"
  title       = "Lanescape Runners infrastructure, project"
  description = "Creates the service's resources and runs Infrastructure Manager's applies. Holds nothing that changes an existing resource."
  permissions = [
    "cloudquotas.quotas.get",
    "config.artifacts.import",
    "config.deployments.deleteState",
    "config.deployments.getLock",
    "config.deployments.getState",
    "config.deployments.updateState",
    "config.previews.upload",
    "config.revisions.getState",
    "logging.logEntries.create",
    "resourcemanager.projects.get",
    "run.operations.get",
    "secretmanager.secrets.create",
    "storage.buckets.create",
  ]
  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "infra_project" {
  project = var.project_id
  role    = google_project_iam_custom_role.infra_project.name
  member  = google_service_account.infra.member
}

# Deploys the service on Cloud Run, granted on it alone (run.tf). It can't
# delete it.
resource "google_project_iam_custom_role" "infra_run" {
  role_id     = "lanescapeRunnersInfraRun"
  title       = "Lanescape Runners infrastructure, Cloud Run"
  description = "Deploys the service's Cloud Run service. Granted only on lanescape-runners."
  permissions = [
    "run.services.get",
    "run.services.getIamPolicy",
    "run.services.setIamPolicy",
    "run.services.update",
  ]
  depends_on = [google_project_service.apis]
}

# --- CD ---------------------------------------------------------------------

# Starts applies and previews of the lanescape-runners deployment, and reads
# their results. Infrastructure Manager can't be limited by name in IAM; the
# deployer can't delete a deployment, and changing one to run as any account
# but lanescape-runners-infra needs acting as that account.
resource "google_project_iam_custom_role" "deployer" {
  role_id     = "lanescapeRunnersDeployer"
  title       = "Lanescape Runners CD"
  description = "Starts applies and previews of the lanescape-runners Infrastructure Manager deployment."
  permissions = [
    "config.deployments.create",
    "config.deployments.get",
    "config.deployments.update",
    "config.operations.get",
    "config.previews.create",
    "config.previews.delete",
    "config.previews.export",
    "config.previews.get",
    "config.revisions.get",
    "config.revisions.list",
  ]
  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "deployer" {
  project = var.project_id
  role    = google_project_iam_custom_role.deployer.name
  member  = google_service_account.deployer.member
}
