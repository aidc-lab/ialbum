package api

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/aidc-lab/ialbum/internal/auth"
)

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	setup, err := s.auth.IsSetup(r.Context())
	if err != nil {
		s.writeError(w, r, 500, "database_error", "无法读取初始化状态", nil)
		return
	}
	s.writeJSON(w, 200, map[string]any{"setupComplete": setup})
}
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var input struct{ Token, Username, Password string }
	if !s.decodeJSON(w, r, &input) {
		return
	}
	user, err := s.auth.Setup(r.Context(), input.Token, input.Username, input.Password)
	if err != nil {
		status := http.StatusUnprocessableEntity
		code := "validation_error"
		if errors.Is(err, auth.ErrSetupClosed) {
			status = http.StatusConflict
			code = "setup_closed"
		} else if errors.Is(err, auth.ErrInvalidSetupToken) {
			status = http.StatusForbidden
			code = "invalid_setup_token"
		}
		s.writeError(w, r, status, code, err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, user)
}
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var input struct{ Username, Password string }
	if !s.decodeJSON(w, r, &input) {
		return
	}
	remoteHost, _, _ := net.SplitHostPort(r.RemoteAddr)
	session, err := s.auth.Login(r.Context(), input.Username, input.Password, remoteHost)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			s.writeError(w, r, http.StatusTooManyRequests, "rate_limited", "登录失败次数过多，请稍后重试", nil)
			return
		}
		s.writeError(w, r, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误", nil)
		return
	}
	s.setCookie(w, session.Token, session.ExpiresAt)
	s.writeJSON(w, 200, map[string]any{"user": map[string]any{"id": session.UserID, "username": session.Username}, "csrfToken": session.CSRFToken, "expiresAt": session.ExpiresAt})
}
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	raw, _ := r.Context().Value(rawTokenKey).(string)
	csrf, err := s.auth.CSRFToken(r.Context(), session.ID, raw)
	if err != nil {
		s.writeError(w, r, 401, "unauthorized", "登录已失效", nil)
		return
	}
	s.writeJSON(w, 200, map[string]any{"user": map[string]any{"id": session.UserID, "username": session.Username}, "csrfToken": csrf, "expiresAt": session.ExpiresAt})
}
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_ = s.auth.Logout(r.Context(), session.ID)
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var input struct{ CurrentPassword, NewPassword string }
	if !s.decodeJSON(w, r, &input) {
		return
	}
	session := sessionFrom(r.Context())
	if err := s.auth.ChangePassword(r.Context(), session.UserID, input.CurrentPassword, input.NewPassword); err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, auth.ErrInvalidCredentials) {
			status = http.StatusUnauthorized
		}
		s.writeError(w, r, status, "password_change_failed", err.Error(), nil)
		return
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func normalizedHost(value string) string {
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		return host
	}
	return strings.Trim(value, "[]")
}
