terraform {
  required_version = ">= 1.14.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.10"
    }
  }

  # bootstrap/'s state bucket, under a prefix of this configuration's own,
  # so neither state holds anything of the other's (README.md).
  backend "gcs" {
    bucket = "lanescape-runners-tfstate"
    prefix = "tools"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
