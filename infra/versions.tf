terraform {
  # Infrastructure Manager's newest Terraform; CD pins it with
  # --tf-version-constraint. No backend: Infrastructure Manager keeps the
  # state.
  required_version = ">= 1.5.7"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.10"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

data "google_project" "this" {
  project_id = var.project_id
}
