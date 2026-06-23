# html-share

社内向け HTML シェア基盤。pon（[BASE の事例](https://devblog.thebase.in/entry/pon)）と同等の構成を GCP + Terraform で再現したもの。

**構成**: GCS（保管・完全非公開）+ Cloud Run（nginx で薄く配信・scale-to-zero）+ IAP（Google Workspace ドメインで社内限定・認証は丸投げ）。
アップロードは **CLI(`share.sh`)** と **Web UI(`/upload`)** の二経路。設計判断の全量は [DESIGN.md](DESIGN.md) を参照。

```
書き手(CLI) ─scripts/share.sh──────────────▶ GCS(<id>/index.html, 非公開)
書き手(Web) ─Googleログイン─▶ IAP ─/upload─▶ │  ▲ uploader(Go, objectCreator で API 書込)
                                    │         │ gcsfuse volume mount (専用SA, read-only)
閲覧者 ──Googleログイン──▶ IAP ──/<id>/──▶ Cloud Run（1サービス2コンテナ）
        (domain:example.com のみ許可)        ├─ nginx    :8080 (ingress, 配信)
                                            └─ uploader :8081 (sidecar, 書込)
```

## 環境（自社の値に置換）

`<...>` は各組織で読み替えるプレースホルダ。下表の例は表記の参考。

| 項目 | 値（プレースホルダ） | 例 |
|---|---|---|
| プロジェクト | `<PROJECT_ID>` | `html-share-prod` |
| リージョン | `<REGION>` | `asia-northeast1`（東京） |
| 共有 URL ベース | `https://<SERVICE_URL>`（Cloud Run が払い出し・`terraform output service_url`） | `https://share-xxxx-an.a.run.app` |
| コンテンツバケット | `<PROJECT_ID>-content`（完全非公開） | `html-share-prod-content` |
| state バケット | `<PROJECT_ID>-tfstate`（versioning + prevent_destroy） | `html-share-prod-tfstate` |
| アクセス許可 | IAP: `domain:<WORKSPACE_DOMAIN>` 一括 | `example.com` |

## 日常の使い方

トップ `https://<SERVICE_URL>/` は**社内向けの紹介ページ**（使い方の入口・[docker/landing/index.html](docker/landing/index.html) をイメージに焼き込み）。アップロードは `/upload`、共有ページは `/<id>/`。いずれも IAP 配下で社内限定。

### Web（非エンジニア向け・単一 HTML）

ブラウザで **`https://share-tqrt3ximjq-an.a.run.app/upload`** を開く（@example.com でログイン）。
HTML をドラッグ＆ドロップすると共有 URL が発行されコピーできる。

- 対応: `.html` / `.htm` の**単一自己完結ファイル**のみ・25MB まで（複数ファイル/ディレクトリは CLI）。
- 作成は誰でも可・**上書き/削除は不可**（毎回新規 ID）。削除は管理者が `unshare.sh`。
- アップロード者のメールと時刻が GCS オブジェクトのメタデータに記録される（IAP 由来＝詐称不可）。

### CLI（ディレクトリ一括・自動化向け）

```bash
cd ~/Documents/html-share
eval "$(cd terraform && terraform output -raw share_env_hint)"

scripts/share.sh ./plan.html          # 単一 HTML
scripts/share.sh ./report_dir/        # HTML + アセット一式（index.html が入口）
# -> https://share-xxxx.a.run.app/<id>/ が出力（@example.com でログイン中の人だけ閲覧可）

scripts/unshare.sh <id>               # 削除
```

閲覧は @example.com の Google アカウントでログインした状態でブラウザから。社外からは到達不可（未認証は Google ログインへリダイレクト）。
アップロード直後は gcsfuse のキャッシュ反映待ちで数秒 404 になることがある（`mount_options` の TTL 短縮で緩和済み）。

## 前提ツール

- `gcloud` / `terraform`(>=1.6) / `docker`（ローカル）。
- ADC 認証: `gcloud auth application-default login`。

## ⚠️ ハマりどころ（再ビルド時に必ず効く）

- **イメージは必ず amd64 でビルド**: Cloud Run は amd64。Apple Silicon の既定（arm64）を push すると起動しない。
  → `docker build --platform linux/amd64 ...`
- **docker push の資格情報ヘルパー**: Homebrew 版 gcloud の `docker-credential-gcloud` が PATH 外のことがある。push 前に通す。
  → `export PATH="/opt/homebrew/share/google-cloud-sdk/bin:$PATH"`
- **state バケット名は `versions.tf` に直書き（別環境では要書き換え）**: [`terraform/versions.tf`](terraform/versions.tf) の
  `backend "gcs" { bucket = "..." }` だけは Terraform 仕様で `var.` を参照できず、`terraform.tfvars` の
  `tfstate_bucket_name` とは別に**ハードコードを手で直す必要がある**（両者は同じ値に揃える）。別プロジェクトに
  作り直すときはここの書き換え漏れに注意（init 時に既存環境の state を見に行ってしまう）。下のステップ3も参照。

## ゼロから再構築する手順（DR / 別環境向け）

> 既存の本番はセットアップ済み。以下は別プロジェクトに作り直す場合の手順。

### 0. プロジェクトと変数

```bash
# 専用プロジェクトを手動作成し請求先を紐付け（org/billing は既存に合わせる）
gcloud projects create <PROJECT_ID> --organization=<ORG_ID> --name="html-share"
gcloud billing projects link <PROJECT_ID> --billing-account=<BILLING_ID>

cp terraform/terraform.tfvars.example terraform/terraform.tfvars
# project_id / iap_domain / *_bucket_name / image を編集
cd terraform
gcloud auth application-default login
```

### 1. 初回 apply（local state）— state バケットとイメージ置き場を先に作る

Cloud Run は `var.image` の実在を要求するため、先にこれらだけ作る。

```bash
terraform init
terraform apply \
  -target=google_project_service.enabled \
  -target=google_storage_bucket.tfstate \
  -target=google_artifact_registry_repository.nginx
```

### 2. イメージ（nginx + uploader）をビルド & push

```bash
export PATH="/opt/homebrew/share/google-cloud-sdk/bin:$PATH"   # ヘルパー対策
gcloud auth configure-docker asia-northeast1-docker.pkg.dev --quiet
REPO="$(cd terraform && terraform output -raw image_repo)"

docker build --platform linux/amd64 -t "$REPO/nginx:1" docker/        && docker push "$REPO/nginx:1"
docker build --platform linux/amd64 -t "$REPO/uploader:1" uploader/   && docker push "$REPO/uploader:1"
# tfvars の image / uploader_image を push したタグに合わせる
```

> 配信(nginx)とアップローダ(uploader)は別イメージ。`docker/nginx.conf` を変えたら nginx タグを、
> `uploader/` を変えたら uploader タグを上げ、`terraform.tfvars` を更新して `terraform apply`。

### 3. state を GCS へ移送（二段階 migrate）

`terraform/versions.tf` の `backend "gcs"` を有効化し `bucket` を `tfstate_bucket_name` に合わせる。
（backend ブロックは `var.` 参照不可なので**直書きを手で書き換える**。上の「ハマりどころ」参照。）

```bash
cd terraform
terraform init -migrate-state
```

### 4. 本 apply

```bash
terraform apply
terraform output share_env_hint
```

## 運用メモ

- **公開事故防止**: content バケットは `uniform_bucket_level_access` + `public_access_prevention=enforced`。GCS 直読みパスは無く、必ず IAP 越し Cloud Run 経由。
- **コスト**: Cloud Run は scale-to-zero。アクセスが無ければほぼ課金されない。
- **gcsfuse**: `mount_options` の `implicit-dirs` でプレースホルダ無しの `<id>/index.html` でも `<id>/` を辿れる。`stat-cache-ttl=5s` / `type-cache-ttl=5s`（既定 60s）で書込直後の一時 404 窓を短縮。軽量 HTML 用途向け（大容量/高頻度には不向き）。
- **権限非対称**: ランタイム SA は当該バケットに `objectViewer`(配信) + `objectCreator`(新規作成のみ)。Web からは上書き/削除不可。削除は CLI(`unshare.sh`)の管理者操作のみ。
- **ページ寿命**: 無期限保持・毎回新規 ID（上書きしない）。自動失効は未設定。
- **state バケット保護**: `prevent_destroy=true`。`terraform destroy` で巻き込まれない。

## 今後（スコープ外）

Slack 連携 / 独自ドメイン（外部 LB + serverless NEG + IAP backend）/ CI/CD apply / 自動失効 / Web からの削除・一覧。
