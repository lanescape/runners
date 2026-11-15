variable "project_id" {
  description = "The project the bucket and its reader are in, as bootstrap/ has it."
  type        = string
  default     = "lanescape-dev"
}

variable "region" {
  description = "The bucket's region, as bootstrap/ has it."
  type        = string
  default     = "northamerica-northeast1"
}

variable "github_owner_id" {
  description = "The organization's ID, as config/organization.yaml and bootstrap/ have it. Only its repositories' tokens may read the bucket."
  type        = number
  default     = 303994381
}
