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
  # backend は var. を参照できないため、bucket は手で書き換える（tfstate_bucket_name と一致させる）。
  # 初回は backend ブロックを丸ごとコメントアウトして local state で apply し、state バケット作成後に有効化する。
  backend "gcs" {
    bucket = "<PROJECT_ID>-tfstate" # 例: html-share-prod-tfstate
    prefix = "html-share"
  }
}
