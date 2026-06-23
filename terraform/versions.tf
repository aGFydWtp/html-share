terraform {
  required_version = ">= 1.6"

  required_providers {
    google = {
      source = "hashicorp/google"
      # iap_enabled (Cloud Run 直付け) と gcs volume mount / mount_options は 6.x が必要
      version = ">= 6.20"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = ">= 6.20"
    }
  }

  # リモート state（二段階ブートストラップ済み）
  backend "gcs" {
    bucket = "html-share-500302-tfstate"
    prefix = "html-share"
  }
}
