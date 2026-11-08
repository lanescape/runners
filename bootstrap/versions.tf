terraform {
  required_version = ">= 1.14.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.10"
    }
  }

  # The bucket is made by hand before the first apply, and adopted by
  # state.tf (README.md). Infrastructure Manager keeps infra/'s state itself.
  backend "gcs" {
    bucket = "lanescape-runners-tfstate"
    prefix = "bootstrap"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

data "google_project" "this" {
  project_id = var.project_id
}
