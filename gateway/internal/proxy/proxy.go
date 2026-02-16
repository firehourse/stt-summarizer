package proxy

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// NewAPIProxy 建立反向代理，將 /api/* 請求轉發至 API Service。
// UserIdentity middleware 已注入 X-User-Id header，此處不需再處理身份識別。
// 路徑轉換：/api/tasks → /tasks（剝除 /api 前綴）。
func NewAPIProxy(apiServiceURL string) http.Handler {
	target, err := url.Parse(apiServiceURL)
	if err != nil {
		log.Fatalf("Invalid API service URL: %v", err)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// 剝除 /api 前綴：/api/tasks → /tasks
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api")
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}

		},
		// 關閉回應緩衝，確保大檔案串流上傳的即時性
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error: %v", err)
			http.Error(w, "Service unavailable", http.StatusBadGateway)
		},
	}

	return proxy
}
