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
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// id は 8 桁 hex（newID と一致）。/mypage/delete のパラメータ検証に使う。
var idRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// 表示は JST 固定（Cloud Run は UTC 稼働のため）。
var jst = time.FixedZone("JST", 9*3600)

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
	mux.HandleFunc("/mypage", s.handleMyPage)        // 自分の共有一覧
	mux.HandleFunc("/mypage/delete", s.handleDelete) // 自分の共有削除（POST）
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

// pageItem は /mypage 一覧の 1 ページ分（<id>/index.html 単位）。
type pageItem struct {
	ID         string
	URL        string
	OrigName   string
	UploadedAt string
}

// handleMyPage は IAP 認証ユーザー本人がアップロードしたページを一覧表示する。
// バケットを走査し metadata.uploader が本人メールと一致する <id>/index.html だけを拾う。
func (s *server) handleMyPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "許可されないメソッドです")
		return
	}
	email := uploaderEmail(r)
	if email == "" {
		writeError(w, http.StatusForbidden, "認証情報を取得できません")
		return
	}

	ctx := r.Context()
	it := s.gcs.Bucket(s.bucket).Objects(ctx, nil)
	var items []pageItem
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			log.Printf("mypage list 失敗: %v", err)
			writeError(w, http.StatusInternalServerError, "一覧の取得に失敗しました")
			return
		}
		if !strings.HasSuffix(attrs.Name, "/index.html") {
			continue // ページの入口（index.html）だけを対象に
		}
		if attrs.Metadata["uploader"] != email {
			continue // 本人のもの以外は除外（IAP メール＝詐称不可）
		}
		id := strings.TrimSuffix(attrs.Name, "/index.html")
		items = append(items, pageItem{
			ID:         id,
			URL:        "/" + id + "/",
			OrigName:   attrs.Metadata["orig_filename"],
			UploadedAt: formatUploadedAt(attrs.Metadata["uploaded_at"], attrs.Created),
		})
	}
	// 新しい順（整形済み "YYYY-MM-DD HH:MM" は辞書順＝時系列）
	sort.Slice(items, func(i, j int) bool { return items[i].UploadedAt > items[j].UploadedAt })

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		Email string
		Items []pageItem
	}{Email: email, Items: items}
	if err := mypageTmpl.Execute(w, data); err != nil {
		log.Printf("mypage render 失敗: %v", err)
	}
}

// handleDelete は本人がアップロードしたページのみ削除する（POST）。
// 削除前に対象オブジェクトの metadata.uploader と IAP メールを厳密照合し、
// 一致しなければ 403。SA は delete 権限を持つため、ここが唯一の本人性チェックになる。
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "許可されないメソッドです")
		return
	}
	email := uploaderEmail(r)
	if email == "" {
		writeError(w, http.StatusForbidden, "認証情報を取得できません")
		return
	}
	id := r.FormValue("id")
	if !idRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "不正な ID です")
		return
	}

	ctx := r.Context()
	obj := s.gcs.Bucket(s.bucket).Object(id + "/index.html")
	attrs, err := obj.Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		writeError(w, http.StatusNotFound, "対象が見つかりません")
		return
	}
	if err != nil {
		log.Printf("delete attrs 取得失敗 (id=%s): %v", id, err)
		writeError(w, http.StatusInternalServerError, "削除に失敗しました")
		return
	}
	// 本人性チェック（email は空を上で弾き済み。他人/メタ無しは不一致で必ず弾かれる）
	if attrs.Metadata["uploader"] != email {
		writeError(w, http.StatusForbidden, "自分がアップロードしたページのみ削除できます")
		return
	}
	if err := obj.Delete(ctx); err != nil {
		log.Printf("delete 失敗 (id=%s): %v", id, err)
		writeError(w, http.StatusInternalServerError, "削除に失敗しました")
		return
	}
	log.Printf("deleted id=%s by %s", id, email)
	http.Redirect(w, r, "/mypage", http.StatusSeeOther)
}

// formatUploadedAt は metadata.uploaded_at(RFC3339) を JST の "YYYY-MM-DD HH:MM" に整形する。
// パースできなければオブジェクト作成時刻、それも無ければ生文字列を返す。
func formatUploadedAt(raw string, fallback time.Time) string {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.In(jst).Format("2006-01-02 15:04")
	}
	if !fallback.IsZero() {
		return fallback.In(jst).Format("2006-01-02 15:04")
	}
	return raw
}

var mypageTmpl = template.Must(template.New("mypage").Parse(mypageHTML))

const mypageHTML = `<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>mypage — html-share</title>
<style>
  :root{
    color-scheme:light;
    --bg:#f6f6f3; --panel:#fff; --ink:#1b1b18; --muted:#70706a; --line:#e4e4de;
    --accent:#38618c; --accent-soft:#eef2f7; --danger:#a23b3b;
    --mono:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,monospace;
    --sans:-apple-system,system-ui,"Hiragino Kaku Gothic ProN","Hiragino Sans","Noto Sans JP",sans-serif;
    --radius:8px;
  }
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--ink);font-family:var(--sans);font-size:15px;line-height:1.6;-webkit-font-smoothing:antialiased}
  a{color:var(--accent)}
  .bar{display:flex;align-items:center;justify-content:space-between;padding:14px 20px;border-bottom:1px solid var(--line);background:var(--panel)}
  .brand{font-size:14px;font-weight:600;color:var(--ink);text-decoration:none;letter-spacing:-.2px;font-family:var(--mono)}
  .brand::before{content:"$ ";color:var(--muted)}
  .nav{display:flex;gap:18px;font-family:var(--mono);font-size:13px}
  .nav a{color:var(--muted);text-decoration:none}
  .nav a:hover{color:var(--accent)}
  main{max-width:760px;margin:0 auto;padding:48px 20px}
  h1{font-size:21px;font-weight:650;margin:0 0 6px;letter-spacing:-.3px}
  .sub{font-size:13.5px;color:var(--muted);margin:0 0 24px}
  .sub .who{font-family:var(--mono)}
  .item{display:flex;align-items:center;gap:14px;background:var(--panel);border:1px solid var(--line);border-radius:var(--radius);padding:13px 16px;margin-bottom:8px}
  .meta{flex:1;min-width:0}
  .meta .name{color:var(--ink);font-weight:600;text-decoration:none;word-break:break-all}
  .meta .name:hover{color:var(--accent)}
  .meta .when{display:block;color:var(--muted);font-family:var(--mono);font-size:12px;margin-top:3px;word-break:break-all}
  .del{background:transparent;color:var(--danger);border:1px solid #e6cfcf;border-radius:6px;padding:6px 12px;font:inherit;font-size:13px;cursor:pointer;white-space:nowrap}
  .del:hover{background:#fbf0f0}
  .empty{color:var(--muted);background:var(--panel);border:1px dashed var(--line);border-radius:var(--radius);padding:40px;text-align:center;font-size:14px}
</style>
</head>
<body>
  <header class="bar">
    <a class="brand" href="/">html-share</a>
    <nav class="nav"><a href="/upload">upload</a><a href="/mypage">mypage</a></nav>
  </header>

  <main>
    <h1>自分の共有</h1>
    <p class="sub"><span class="who">{{.Email}}</span> がアップロードしたページ · <a href="/upload">新規アップロード</a></p>
    {{if .Items}}
      {{range .Items}}
      <div class="item">
        <div class="meta">
          <a class="name" href="{{.URL}}" target="_blank" rel="noopener">{{if .OrigName}}{{.OrigName}}{{else}}{{.ID}}{{end}}</a>
          <span class="when">{{.URL}} · {{.UploadedAt}}</span>
        </div>
        <form method="post" action="/mypage/delete" onsubmit="return confirm('このページを削除します。元に戻せません。よろしいですか？')">
          <input type="hidden" name="id" value="{{.ID}}">
          <button class="del" type="submit">削除</button>
        </form>
      </div>
      {{end}}
    {{else}}
      <div class="empty">まだ共有ページがありません。<a href="/upload">アップロード</a>してみましょう。</div>
    {{end}}
  </main>
</body>
</html>
`
