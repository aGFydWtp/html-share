# --- build stage ---
# BUILDPLATFORM 固定でビルダはホスト native 実行（arm64 Mac でも emulation 回避）。
# 出力は下の GOARCH=amd64 でクロスコンパイルするため Cloud Run(amd64)向け。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# 依存だけ先に解決（キャッシュ効率）
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 静的バイナリ（distroless/static で動かすため CGO 無効・amd64 固定）
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /uploader .

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /uploader /uploader
EXPOSE 8081
ENTRYPOINT ["/uploader"]
