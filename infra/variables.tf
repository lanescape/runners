variable "project_id" {
  description = "The project everything runs in, shared with other teams."
  type        = string
  default     = "lanescape-dev"
}

variable "region" {
  description = "The region of the service and the proxy."
  type        = string
  default     = "northamerica-northeast1"
}

variable "proxy_git_zone" {
  description = "The zone of the proxy's git machine. bootstrap/ limits the infra identity to this zone."
  type        = string
  default     = "northamerica-northeast1-a"
}

variable "github_app_id" {
  description = "The numeric ID of the \"Lanescape Runners\" GitHub App."
  type        = number

  validation {
    condition     = var.github_app_id > 0 && floor(var.github_app_id) == var.github_app_id
    error_message = "github_app_id must be the App's numeric ID."
  }
}

# The images CD has just pushed, each by digest, so an apply deploys exactly
# what CD built. Each must be in the service's own repository.

variable "service_image" {
  description = "The service's image, by digest."
  type        = string

  validation {
    condition     = can(regex("^northamerica-northeast1-docker\\.pkg\\.dev/lanescape-dev/lanescape-runners/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$", var.service_image))
    error_message = "service_image must be an image in the lanescape-runners repository, by digest."
  }
}

variable "proxy_image" {
  description = "The HTTP proxy's image, by digest."
  type        = string

  validation {
    condition     = can(regex("^northamerica-northeast1-docker\\.pkg\\.dev/lanescape-dev/lanescape-runners/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$", var.proxy_image))
    error_message = "proxy_image must be an image in the lanescape-runners repository, by digest."
  }
}

variable "proxy_git_image" {
  description = "The proxy's git server's image, by digest, for linux/amd64."
  type        = string

  validation {
    condition     = can(regex("^northamerica-northeast1-docker\\.pkg\\.dev/lanescape-dev/lanescape-runners/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$", var.proxy_git_image))
    error_message = "proxy_git_image must be an image in the lanescape-runners repository, by digest."
  }
}

variable "proxy_git_machine_type" {
  description = "The git machine's machine type."
  type        = string
  default     = "e2-medium"
}

variable "proxy_git_disk_gb" {
  description = "The size of the git machine's mirrors disk."
  type        = number
  default     = 200
}
