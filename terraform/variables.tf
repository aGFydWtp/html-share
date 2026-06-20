variable "project_id" {
  description = "リソース収容先の専用プロジェクト ID（手動作成済み）"
  type        = string
}

variable "region" {
  description = "Cloud Run / GCS / Artifact Registry のリージョン"
  type        = string
  default     = "asia-northeast1"
}

variable "iap_domain" {
  description = "アクセスを許可する Google Workspace ドメイン（例: example.com）。IAP は domain: 単位で一括許可"
  type        = string
}

variable "content_bucket_name" {
  description = "共有 HTML を保管するバケット名（グローバル一意）"
  type        = string
}

variable "tfstate_bucket_name" {
  description = "Terraform state を保管するバケット名（グローバル一意）。backend.gcs.bucket と一致させる"
  type        = string
}

variable "image" {
  description = "Cloud Run にデプロイする nginx イメージの完全参照（例: asia-northeast1-docker.pkg.dev/PROJ/html-share/nginx:1）"
  type        = string
}

variable "uploader_image" {
  description = "Web アップローダ(sidecar)イメージの完全参照（例: asia-northeast1-docker.pkg.dev/PROJ/html-share/uploader:1）"
  type        = string
}

variable "service_name" {
  description = "Cloud Run サービス名（URL の prefix になる）"
  type        = string
  default     = "share"
}
