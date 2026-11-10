variable "project_id" {
  description = "The project everything runs in. Other teams' resources are there too, so everything here is named lanescape-runners-* or lsr-*."
  type        = string
  default     = "lanescape-dev"
}

variable "region" {
  description = "The region of the service, its machines and its buckets."
  type        = string
  default     = "northamerica-northeast1"
}

variable "zones" {
  description = "The zones machines are made in, in order; config/shapes.yaml lists the same. The service's grants are limited to them."
  type        = list(string)
  default     = ["northamerica-northeast1-a", "northamerica-northeast1-b"]

  # Each zone adds an operator to the service's conditions, which IAM limits
  # to 12 (roles.tf).
  validation {
    condition     = length(var.zones) >= 1 && length(var.zones) <= 8
    error_message = "zones must list between 1 and 8 zones."
  }
}

variable "github_owner_id" {
  description = "The organization's ID, as config/organization.yaml has it. CD's sign-in checks it."
  type        = number
  default     = 303994381
}

variable "github_repository_id" {
  description = "This repository's ID (gh api repos/lanescape/runners --jq .id). CD's sign-in checks it."
  type        = number
  default     = 1450359106
}

variable "machines_cidr" {
  description = "The range of the subnetwork the service's machines join."
  type        = string
  default     = "10.64.0.0/20"
}
