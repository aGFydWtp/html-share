#!/usr/bin/env bash
# 共有ページを ID 単位で削除する。
#   SHARE_BUCKET=<PROJECT_ID>-content scripts/unshare.sh <id>
set -euo pipefail

: "${SHARE_BUCKET:?set SHARE_BUCKET}"
id="${1:?usage: SHARE_BUCKET=... scripts/unshare.sh <id>}"

gcloud storage rm --recursive "gs://${SHARE_BUCKET}/${id}/"
echo "deleted: ${id}"
