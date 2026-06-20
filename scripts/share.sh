#!/usr/bin/env bash
# HTML(単一ファイル or ディレクトリ)を GCS に上げ、ランダム ID の共有 URL を発行する。
#
#   SHARE_BUCKET=html-share-prod-content \
#   SHARE_BASE_URL=https://share-xxxx.a.run.app \
#   scripts/share.sh ./report.html
#
# (env は `terraform output share_env_hint` の出力をそのまま使える)
set -euo pipefail

: "${SHARE_BUCKET:?set SHARE_BUCKET (例: html-share-prod-content)}"
: "${SHARE_BASE_URL:?set SHARE_BASE_URL (例: https://share-xxxx.a.run.app)}"

src="${1:-}"
if [[ -z "$src" ]]; then
  echo "usage: SHARE_BUCKET=... SHARE_BASE_URL=... scripts/share.sh <file.html|dir>" >&2
  exit 1
fi
if [[ ! -e "$src" ]]; then
  echo "error: '$src' が存在しません" >&2
  exit 1
fi

id="$(openssl rand -hex 4)"
dest="gs://${SHARE_BUCKET}/${id}"

if [[ -d "$src" ]]; then
  if [[ ! -f "${src%/}/index.html" ]]; then
    echo "warning: '${src%/}/index.html' が無いため閲覧時 404 になります" >&2
  fi
  # ディレクトリの中身を <id>/ 配下へ
  gcloud storage cp --recursive "${src%/}/." "${dest}/"
else
  # 単一ファイルは <id>/index.html として配置
  gcloud storage cp "$src" "${dest}/index.html"
fi

url="${SHARE_BASE_URL%/}/${id}/"
echo "$url"
if command -v pbcopy >/dev/null 2>&1; then
  printf '%s' "$url" | pbcopy && echo "(クリップボードにコピーしました)"
fi
