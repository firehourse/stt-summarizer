package middleware

import (
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	cookieName   = "userId"
	cookieMaxAge = 365 * 24 * 60 * 60 // 1 year
	headerUserID = "X-User-Id"
)

// UserIdentity 匿名用戶識別 middleware。
// 從 Cookie 讀取 userId（不存在則核發新的 UUID），
// 注入 X-User-Id header 供下游 API Service 使用。API Service 信任此 header，不處理 Cookie。
func UserIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var userID string

		cookie, err := r.Cookie(cookieName)
		if err != nil || cookie.Value == "" {
			userID = uuid.New().String()
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    userID,
				Path:     "/",
				HttpOnly: true,
				MaxAge:   cookieMaxAge,
				Expires:  time.Now().Add(time.Duration(cookieMaxAge) * time.Second),
			})
		} else {
			userID = cookie.Value
		}

		r.Header.Set(headerUserID, userID)

		next.ServeHTTP(w, r)
	})
}
