FROM nginx:1.27-alpine

# 既定の server ブロックを差し替え
COPY nginx.conf /etc/nginx/conf.d/default.conf
# カスタムエラーページ
COPY errors/404.html /usr/share/nginx/errors/404.html
# トップの社内向け紹介ページ（"/" で配信）
COPY landing/index.html /usr/share/nginx/landing/index.html

EXPOSE 8080
