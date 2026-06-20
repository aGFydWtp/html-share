locals {
  required_apis = [
    "run.googleapis.com",
    "iap.googleapis.com",
    "storage.googleapis.com",
    "artifactregistry.googleapis.com",
  ]
}

resource "google_project_service" "enabled" {
  for_each           = toset(local.required_apis)
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# ---------- Artifact Registry（nginx イメージ置き場）----------
resource "google_artifact_registry_repository" "nginx" {
  project       = var.project_id
  location      = var.region
  repository_id = "html-share"
  format        = "DOCKER"
  description   = "html-share 配信用 nginx イメージ"

  depends_on = [google_project_service.enabled]
}

# ---------- Terraform state バケット（二段階 migrate で本構成が管理）----------
resource "google_storage_bucket" "tfstate" {
  name                        = var.tfstate_bucket_name
  project                     = var.project_id
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false

  versioning {
    enabled = true
  }

  # state 置き場を terraform destroy で巻き込まないための保険
  lifecycle {
    prevent_destroy = true
  }
}

# ---------- コンテンツ（共有 HTML）バケット：完全非公開 ----------
resource "google_storage_bucket" "content" {
  name                        = var.content_bucket_name
  project                     = var.project_id
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false

  depends_on = [google_project_service.enabled]
}

# ---------- Cloud Run 実行用サービスアカウント（最小権限）----------
resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = "${var.service_name}-runtime"
  display_name = "html-share Cloud Run runtime"
}

# 当該コンテンツバケットのみ閲覧可（gcsfuse はこの SA 権限で読む）
resource "google_storage_bucket_iam_member" "runtime_object_viewer" {
  bucket = google_storage_bucket.content.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.runtime.email}"
}

# Web アップローダ(sidecar)用。新規オブジェクト作成のみ可（上書き/削除は不可・W5/W9）。
# objectViewer と objectCreator の合算で「作成は誰でも / 削除は管理者 CLI のみ」の権限非対称を担保。
resource "google_storage_bucket_iam_member" "runtime_object_creator" {
  bucket = google_storage_bucket.content.name
  role   = "roles/storage.objectCreator"
  member = "serviceAccount:${google_service_account.runtime.email}"
}

# ---------- Cloud Run サービス（nginx + GCS volume mount）----------
resource "google_cloud_run_v2_service" "share" {
  name     = var.service_name
  project  = var.project_id
  location = var.region

  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = false

  # IAP を Cloud Run に直付け（LB 不要）
  iap_enabled = true

  template {
    execution_environment = "EXECUTION_ENVIRONMENT_GEN2"
    service_account       = google_service_account.runtime.email

    scaling {
      min_instance_count = 0
      max_instance_count = 2
    }

    # ingress: nginx（配信・8080）。ports を持つコンテナが ingress 扱いになる。
    containers {
      name  = "nginx"
      image = var.image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }

      volume_mounts {
        name       = "content"
        mount_path = "/srv/share"
      }
    }

    # sidecar: Go アップローダ（localhost:8081 で書込受付・W3/W4）。ports なし＝非 ingress。
    containers {
      name  = "uploader"
      image = var.uploader_image

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }

      env {
        name  = "SHARE_BUCKET"
        value = google_storage_bucket.content.name
      }
      env {
        name  = "PORT"
        value = "8081"
      }
    }

    volumes {
      name = "content"
      gcs {
        bucket    = google_storage_bucket.content.name
        read_only = true
        # implicit-dirs: 末尾オブジェクト(プレースホルダ無し)でも <id>/ を辿れるように。
        # stat/type-cache-ttl: 既定 60s を 5s に短縮し、書込直後の一時 404 窓を縮める（W11）。
        mount_options = ["implicit-dirs", "stat-cache-ttl=5s", "type-cache-ttl=5s"]
      }
    }
  }

  depends_on = [google_project_service.enabled]
}

# ---------- IAP サービスエージェント（明示作成）----------
resource "google_project_service_identity" "iap" {
  provider = google-beta
  project  = var.project_id
  service  = "iap.googleapis.com"

  depends_on = [google_project_service.enabled]
}

# IAP が Cloud Run を呼び出すための invoker 権限
resource "google_cloud_run_v2_service_iam_member" "iap_invoker" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.share.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_project_service_identity.iap.email}"
}

# ---------- IAP アクセス許可：ドメイン一括のみ ----------
resource "google_iap_web_cloud_run_service_iam_member" "domain_access" {
  project                = var.project_id
  location               = var.region
  cloud_run_service_name = google_cloud_run_v2_service.share.name
  role                   = "roles/iap.httpsResourceAccessor"
  member                 = "domain:${var.iap_domain}"
}
