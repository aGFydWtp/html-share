package main

import _ "embed"

// アップロード UI（単一 HTML + 最小 JS）。バイナリに同梱して GET /upload で配信する。
//
//go:embed index.html
var indexHTML []byte
