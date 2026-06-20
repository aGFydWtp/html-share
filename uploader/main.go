// html-share Web アップローダ（サイドカー）。
//
// 役割: ブラウザから単一 HTML を受け取り、GCS の <id>/index.html として
// 新規作成（上書き不可）し、共有 URL を返す。配信は同居する nginx が担当。
//
// 思想（DESIGN.md W1-W13）:
//   - net/http + GCS SDK のみ。UI は embed 同梱。
//   - 書込権限は objectCreator のみ → DoesNotExist 条件で作成、衝突時は採番し直し。
//   - .html/.htm のみ・25MB 上限・サニタイズなし（社内 IAP 配下）。
//   - uploader メール/時刻を GCS カスタムメタデータに付与（IAP ヘッダ由来＝詐称不可）。
//   - 返却 URL は Host ヘッダから導出（バケット名のみ env 注入）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

const (
	maxUploadBytes = 25 << 20 // 25MB（W6）
	maxIDRetries   = 5        // ID 衝突時の採番リトライ上限（W5）
)

type server struct {
	gcs    *storage.Client
	bucket string
}

func main() {
	bucket := os.Getenv("SHARE_BUCKET")
	if bucket == "" {
		log.Fatal("SHARE_BUCKET が未設定です")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081" // nginx が localhost:8081 へ proxy（W3）
	}

	ctx := context.Background()
	gcs, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("GCS クライアント初期化に失敗: %v", err)
	}
	defer gcs.Close()

	s := &server{gcs: gcs, bucket: bucket}

	mux := http.NewServeMux()
	mux.HandleFunc("/upload", s.handleUpload)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("uploader listening on :%s (bucket=%s)", port, bucket)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// D&D フォーム（embed 同梱の単一 HTML）
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "許可されないメソッドです")
	}
}

func (s *server) handlePost(w http.ResponseWriter, r *http.Request) {
	// 25MB 上限（multipart 全体に上限を掛ける）
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "ファイルが大きすぎます（上限 25MB）")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ファイルが選択されていません")
		return
	}
	defer file.Close()

	// 拡張子検証（.html / .htm のみ・W6）
	name := strings.ToLower(header.Filename)
	if !strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".htm") {
		writeError(w, http.StatusUnsupportedMediaType, ".html / .htm のみアップロードできます")
		return
	}

	// 本文を一旦メモリへ（リトライ時に再読込するため）。上限内なので許容。
	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ファイル読込に失敗しました")
		return
	}
	if len(data) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "ファイルが大きすぎます（上限 25MB）")
		return
	}

	email := uploaderEmail(r)
	meta := map[string]string{
		"uploaded_at":   time.Now().UTC().Format(time.RFC3339),
		"orig_filename": header.Filename,
	}
	if email != "" {
		meta["uploader"] = email
	}

	id, err := s.createWithFreshID(r.Context(), data, meta)
	if err != nil {
		log.Printf("upload 失敗: %v", err)
		writeError(w, http.StatusInternalServerError, "アップロードに失敗しました")
		return
	}

	// 返却 URL は Host ヘッダから導出（同一ホスト・IAP 経由・W13）
	url := "https://" + r.Host + "/" + id + "/"
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "id": id})
}

// createWithFreshID は ID を採番し、DoesNotExist 条件で <id>/index.html を作成する。
// ID 衝突（412）時は採番し直してリトライする（W5）。
func (s *server) createWithFreshID(ctx context.Context, data []byte, meta map[string]string) (string, error) {
	var lastErr error
	for i := 0; i < maxIDRetries; i++ {
		id, err := newID()
		if err != nil {
			return "", err
		}
		obj := s.gcs.Bucket(s.bucket).Object(id + "/index.html")
		// DoesNotExist=true → ifGenerationMatch=0。既存なら 412 で弾かれる（上書き不可）。
		wc := obj.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
		wc.ContentType = "text/html; charset=utf-8"
		wc.Metadata = meta

		if _, err := wc.Write(data); err != nil {
			_ = wc.Close()
			lastErr = err
			continue
		}
		if err := wc.Close(); err != nil {
			lastErr = err
			if isPreconditionFailed(err) {
				continue // ID 衝突 → 採番し直し
			}
			return "", err // それ以外（権限等）は即エラー
		}
		return id, nil
	}
	return "", lastErr
}

// newID は CLI(share.sh)と統一の 8 桁 hex（4 バイト乱数）を返す。
func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// uploaderEmail は IAP が付与する認証済みメールを取り出す（詐称不可・W12）。
// 値の形式: "accounts.google.com:user@example.com"
func uploaderEmail(r *http.Request) string {
	v := r.Header.Get("X-Goog-Authenticated-User-Email")
	if v == "" {
		return ""
	}
	if i := strings.LastIndex(v, ":"); i >= 0 {
		return v[i+1:]
	}
	return v
}

func isPreconditionFailed(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == http.StatusPreconditionFailed // 412
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
