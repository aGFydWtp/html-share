# html-share 設計メモ（pon 同等の社内 HTML シェア基盤 / GCP + Terraform）

参照元: https://devblog.thebase.in/entry/pon （BASE「社内 HTML ホスティング」）
本質: **GCS（保管）+ Cloud Run（薄い配信）+ IAP（社内認証を丸投げ）**。
思想: 薄く・低コスト・運用レス、認証を自前実装しない、「うっかり全世界公開」を構造的に潰す。

## 確定した設計判断

| # | 論点 | 決定 |
|---|------|------|
| 1 | クラウド | **GCP**（pon を忠実再現。IAP のゼロ実装認証が核） |
| 2 | ID 基盤 | **Google Workspace + GCP Organization あり**。会社の正式な社内ツール |
| 3 | アップロード方式 / URL | **CLI + Web の二経路 + ランダム ID**（`/<id>/`）。CLI(`share.sh`)は併存、Web は非エンジニア向けに単一 HTML を上げる導線（→「Web アップロード UI」節） |
| 4 | 配信実装 | **Cloud Run gen2 + GCS volume mount + 素の nginx**（自前コードほぼゼロ） |
| 5 | ドメイン / URL | **デフォルト `run.app` のまま IAP を Cloud Run 直付け**（LB なし・最薄） |
| 6 | プロジェクト / state | **専用プロジェクト**（手動作成）+ **GCS リモート state**（バケットは Terraform 管理） |
| 6b | state ブートストラップ | **二段階 migrate**（local apply →backend 設定 →`init -migrate-state`）。バケットに `prevent_destroy` |
| 7 | イメージ / apply | **Artifact Registry** + **ローカル `docker build` & push** + **手動 apply**（CI 化は後） |
| 8 | URL 発行体験 | **`share.sh <file_or_dir>`**（ID 採番→アップロード→URL 出力）。Web UI は別途 |
| 9 | アップロード単位 / ルーティング | **ディレクトリ基本（単一 HTML も内包）**。`/<id>/`→`index.html` 正規化、配下は相対パス配信 |
| 10 | バケット安全 | **完全非公開**（uniform bucket-level access + public access prevention = enforced）。専用 SA に当該バケットのみ `objectViewer`（配信）＋ Web 追加に伴い `objectCreator`（新規作成のみ・上書き/削除不可）を限定追加（→「Web アップロード UI」節） |
| 11 | IAP アクセス / 監査 | **`domain:` 一括のみ・例外なし** + **IAP アクセスログ有効** |
| 12 | ページ寿命 | **無期限保持** / **削除スクリプト** / **毎回新規 ID（上書きしない）** |
| 13 | リージョン / 構成 | **asia-northeast1（東京）** / **単一ルートモジュール** |

## アーキテクチャ

```
書き手(CLI) ──share.sh──────────────────────▶ GCS バケット（完全非公開）
書き手(Web) ─Googleログイン─▶ IAP ─/upload─▶ │  gs://.../<id>/index.html
                                    │         ▲ (Go: objectCreator で API 書込)
                                    │         │ (GCS volume mount[read_only],
                                    │         │  専用SA objectViewer で nginx 読取)
閲覧者 ──Googleログイン──▶ IAP ──/<id>/──▶ Cloud Run サービス (scale-to-zero)
        (domain:company.com のみ許可)        ├─ nginx     :8080 (ingress, 配信)
                                             └─ uploader  :8081 (sidecar, Go)
```

Cloud Run は **1 サービス・2 コンテナ**。nginx が ingress(8080)で配信を担い、`/upload` 前綴だけを
localhost:8081 の Go サイドカーへ `proxy_pass`。IAP は 1 つのサービスに 1 回設定するだけで配信・
アップロード双方を保護する（URL も 1 本）。

## リポジトリ構成

```
html-share/
  terraform/     # providers / backend / bucket / cloudrun / iap / iam / variables / outputs
  docker/        # nginx 用 Dockerfile + nginx.conf（/<id>/ → index.html、/upload → sidecar）
  uploader/      # Go アップローダ: main.go + 埋め込み(index.html) + Dockerfile（distroless）
  scripts/       # share.sh（採番+アップロード+URL）/ unshare.sh（削除）
  README.md
```

## Web アップロード UI（決定）

非エンジニアが「ブラウザで HTML を D&D → 共有 URL が出る」導線。既存の配信サービスに
**サイドカーとして同居**させ、思想（薄く・事故らない・認証は IAP 丸投げ）を維持する。
CLI(`share.sh`)は廃止せず併存（ディレクトリ一括や自動化は引き続き CLI 担当）。

| # | 論点 | 決定 |
|---|------|------|
| W1 | 利用者 / CLI | 非エンジニア向けに Web 追加、`share.sh` は残す（役割分担） |
| W2 | アップロード単位 | **単一 HTML 自己完結ファイルのみ**（複数/ディレクトリは CLI） |
| W3 | 構成 | 既存 Cloud Run に**サイドカー同居**（1 サービス・1 URL・1 IAP）。nginx=配信(8080 ingress) / Go=書込(localhost:8081) |
| W4 | 言語 / イメージ | **Go**（`net/http` + GCS SDK のみ、UI は `embed` 同梱）。`distroless/static` で極小・コールドスタート優先 |
| W5 | 書込権限 | ランタイム SA に **`objectCreator` のみ追加**（新規作成のみ・上書き/削除不可）。`ifGenerationMatch=0` で作成、ID 衝突時は採番し直してリトライ。ID は 8 桁 hex で CLI と統一 |
| W6 | 入力検証 | **`.html`/`.htm` のみ・25MB 上限・サニタイズなし**（社内 IAP 配下・作成者=被害者が同一境界のため過剰防御しない） |
| W7 | 経路 | **`/upload`**（GET=D&D フォーム / POST=受信）。nginx が前綴のみ sidecar へ proxy、ルート(`/`)は従来どおり 404 維持。ID は `[0-9a-f]{8}` で `upload` と衝突しない |
| W8 | UX | 単一 HTML + 最小 JS（D&D・進捗・URL 表示・コピーボタン・エラー表示）。フレームワーク無し |
| W9 | 削除 | **Web スコープ外**（削除は `unshare.sh`）。「作成は誰でも / 削除は管理者 CLI のみ」の権限非対称を IAM で担保 |
| W10 | デプロイ | **2 イメージ・手動 build/push・手動 apply・マルチコンテナ維持**（決定 #7 踏襲、CI 化は後） |
| W11 | 反映遅延対策 | gcsfuse の**メタデータキャッシュ TTL を短縮**（gcs volume `mount_options`）+ 完了画面に「反映に数秒」の注意書き |
| W12 | 監査 | GCS オブジェクトの**カスタムメタデータに uploader メール + 時刻**を付与（IAP `X-Goog-Authenticated-User-Email` 由来＝詐称不可） |
| W13 | 返却 URL | 環境変数ではなく**リクエストの Host ヘッダから導出**（同一ホスト・IAP 経由）。バケット名のみ env 注入 |

実装で触る範囲: `uploader/`（新規 Go）, `docker/nginx.conf`（`/upload` の `proxy_pass`）,
`terraform/main.tf`（sidecar `containers`・`objectCreator` IAM・`uploader_image` 変数・gcs volume の
`mount_options` TTL）, `terraform/variables.tf` / `outputs.tf`, `README.md`。

## 既知の留意点
- GCS volume mount（gcsfuse）は大容量/高頻度でレイテンシ・コスト特性が SDK と異なる。本用途（軽量 HTML・低頻度）では許容。
- **アップロード直後の配信反映**: Go は GCS API で書き、nginx は gcsfuse(read_only) で読むため、メタデータキャッシュ次第で書込直後に一時 404 になりうる。`mount_options` の TTL 短縮で緩和（W11）。
- IAP 直付けのため URL は `run.app`。独自ドメインが要件化したら「外部 LB + serverless NEG + IAP backend」へ移行（構成は一段厚くなる）。
- `terraform destroy` は state バケットを巻き込まないよう `prevent_destroy` で保護。

## 今後の拡張（今回スコープ外）
Slack 連携、独自ドメイン、CI/CD apply、自動失効、同一 URL 更新、Web からの削除/一覧。
