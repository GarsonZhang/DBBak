package api

import (
	"net/http"
	"strings"
	"time"

	"vault/internal/security"
)

// login 校验管理员并设置 HttpOnly 会话 Cookie；响应不包含密码或会话明文。
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	user, hash, err := s.store.Authenticate(r.Context(), input.Username)
	if err != nil || !security.VerifyPassword(hash, input.Password) {
		s.audit(r, "login", input.Username, "denied")
		s.fail(w, r, http.StatusUnauthorized, "LOGIN_FAILED", "用户名或密码错误")
		return
	}
	token, err := security.NewToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	expires := time.Now().UTC().Add(s.config.SessionTTL)
	if err = s.store.CreateSession(r.Context(), security.TokenHash(token), user.ID, expires); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "vault_session", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(s.config.SessionTTL.Seconds()), Expires: expires})
	s.audit(r, "login", user.ID, "success")
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "expires_at": expires})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := requestToken(r)
	if token != "" {
		_ = s.store.DeleteSession(r.Context(), security.TokenHash(token))
	}
	http.SetCookie(w, &http.Cookie{Name: "vault_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	s.audit(r, "logout", actor(r.Context()), "success")
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}
