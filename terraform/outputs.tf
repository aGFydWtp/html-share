output "service_url" {
  description = "共有 URL のベース（このURL + /<id>/ が各ページ）"
  value       = google_cloud_run_v2_service.share.uri
}

output "upload_url" {
  description = "ブラウザで開く Web アップロード画面（IAP 認証後に D&D）"
  value       = "${google_cloud_run_v2_service.share.uri}/upload"
}

output "content_bucket" {
  description = "share.sh に渡すコンテンツバケット名"
  value       = google_storage_bucket.content.name
}

output "image_repo" {
  description = "docker push 先の Artifact Registry リポジトリ"
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.nginx.repository_id}"
}

output "share_env_hint" {
  description = "scripts/share.sh 用の環境変数 export 文"
  value       = "export SHARE_BUCKET=${google_storage_bucket.content.name} SHARE_BASE_URL=${google_cloud_run_v2_service.share.uri}"
}
