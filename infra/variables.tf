variable "project_id" {
  description = "The project everything runs in, shared with other teams."
  type        = string
  default     = "lanescape-dev"
}

variable "region" {
  description = "The region of the service and its bucket."
  type        = string
  default     = "northamerica-northeast1"
}

variable "github_app_id" {
  description = "The numeric ID of the \"Lanescape Runners\" GitHub App."
  type        = number

  validation {
    condition     = var.github_app_id > 0 && floor(var.github_app_id) == var.github_app_id
    error_message = "github_app_id must be the App's numeric ID."
  }
}

# The image CD has just pushed, by digest, so an apply deploys exactly what
# CD built. It must be in the service's own repository.

variable "service_image" {
  description = "The service's image, by digest."
  type        = string

  validation {
    condition     = can(regex("^northamerica-northeast1-docker\\.pkg\\.dev/lanescape-dev/lanescape-runners/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$", var.service_image))
    error_message = "service_image must be an image in the lanescape-runners repository, by digest."
  }
}
