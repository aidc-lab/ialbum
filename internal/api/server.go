package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/aidc-lab/ialbum/internal/auth"
	"github.com/aidc-lab/ialbum/internal/config"
	appdb "github.com/aidc-lab/ialbum/internal/db"
	"github.com/aidc-lab/ialbum/internal/jobs"
	"github.com/aidc-lab/ialbum/internal/media"
	storagemanager "github.com/aidc-lab/ialbum/internal/storage/manager"
)

type Server struct {
	cfg            config.Config
	db             *appdb.DB
	auth           *auth.Service
	storages       *storagemanager.Manager
	catalog        *media.Service
	processor      *media.Processor
	jobs           *jobs.Queue
	logger         *slog.Logger
	assets         fs.FS
	downloadSlots  chan struct{}
	requestCounter atomic.Uint64
}
type contextKey string

const (
	sessionKey  contextKey = "session"
	rawTokenKey contextKey = "raw-session-token"
)

func New(cfg config.Config, db *appdb.DB, authService *auth.Service, storages *storagemanager.Manager, catalog *media.Service, processor *media.Processor, queue *jobs.Queue, logger *slog.Logger, assetFS fs.FS) *Server {
	return &Server{cfg: cfg, db: db, auth: authService, storages: storages, catalog: catalog, processor: processor, jobs: queue, logger: logger, assets: assetFS, downloadSlots: make(chan struct{}, cfg.DownloadConcurrency)}
}
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, s.requestID, s.securityHeaders)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/setup/status", s.handleSetupStatus)
		r.Post("/setup", s.handleSetup)
		r.Post("/auth/login", s.handleLogin)
		r.Group(func(r chi.Router) {
			r.Use(s.requireSession)
			r.Get("/auth/me", s.handleMe)
			r.Group(func(r chi.Router) {
				r.Use(s.requireCSRF)
				r.Delete("/auth/session", s.handleLogout)
				r.Put("/settings/account/password", s.handleChangePassword)
				r.Get("/storage-connections", s.handleListStorages)
				r.Post("/storage-connections", s.handleCreateStorage)
				r.Get("/storage-connections/{id}", s.handleGetStorage)
				r.Patch("/storage-connections/{id}", s.handleUpdateStorage)
				r.Post("/storage-connections/{id}/test", s.handleTestStorage)
				r.Delete("/storage-connections/{id}", s.handleDeleteStorage)
				r.Post("/baidu/device-flows", s.handleStartBaiduFlow)
				r.Delete("/baidu/device-flows/{id}", s.handleCancelBaiduFlow)
				r.Post("/albums", s.handleCreateAlbum)
				r.Patch("/albums/{id}", s.handleUpdateAlbum)
				r.Delete("/albums/{id}", s.handleDeleteAlbum)
				r.Post("/albums/{id}/scan", s.handleScanAlbum)
				r.Put("/albums/{id}/cover", s.handleSetCover)
				r.Post("/albums/{id}/media", s.handleUpload)
				r.Post("/download-tickets", s.handleCreateDownloadTicket)
				r.Post("/jobs/{id}/retry", s.handleRetryJob)
				r.Post("/media/{id}/pending-delete/cancel", s.handleCancelDelete)
				r.Post("/media/{id}/pending-delete/resume", s.handleResumeDelete)
			})
			r.Get("/baidu/device-flows/{id}", s.handleGetBaiduFlow)
			r.Get("/baidu/device-flows/{id}/qr", s.handleBaiduFlowQR)
			r.Get("/storage-connections/{id}/objects", s.handleBrowseStorage)
			r.MethodFunc(http.MethodGet, "/storage-connections/{id}/content", s.handleStorageObjectContent)
			r.MethodFunc(http.MethodHead, "/storage-connections/{id}/content", s.handleStorageObjectContent)
			r.Get("/albums", s.handleListAlbums)
			r.Get("/albums/{id}", s.handleGetAlbum)
			r.Get("/albums/{id}/media", s.handleListMedia)
			r.MethodFunc(http.MethodGet, "/media/{id}/content", s.handleMediaContent)
			r.MethodFunc(http.MethodHead, "/media/{id}/content", s.handleMediaContent)
			r.Get("/media/{id}/thumbnails/{variant}", s.handleThumbnail)
			r.Get("/downloads/{token}", s.handleDownload)
			r.Get("/jobs", s.handleListJobs)
			r.Get("/settings/system", s.handleSystemSettings)
		})
	})
	r.NotFound(s.serveSPA)
	return r
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("req-%d-%d", time.Now().UnixMilli(), s.requestCounter.Add(1))
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey("request-id"), id)))
	})
}
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("ialbum_session")
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "请先登录", nil)
			return
		}
		session, err := s.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			s.clearCookie(w)
			s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "登录已失效", nil)
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey, session)
		ctx = context.WithValue(ctx, rawTokenKey, cookie.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		session := sessionFrom(r.Context())
		token := r.Header.Get("X-CSRF-Token")
		if err := s.auth.ValidateCSRF(r.Context(), session.ID, token); err != nil {
			s.writeError(w, r, http.StatusForbidden, "invalid_csrf", "安全令牌无效，请刷新页面", nil)
			return
		}
		if !s.validOrigin(r) {
			s.writeError(w, r, http.StatusForbidden, "invalid_origin", "请求来源无效", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) validOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Referer") == "" || strings.HasPrefix(r.Header.Get("Referer"), s.expectedOrigin(r))
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	expected, _ := url.Parse(s.expectedOrigin(r))
	if parsed.Host == expected.Host && parsed.Scheme == expected.Scheme {
		return true
	}
	originHost, _, _ := net.SplitHostPort(parsed.Host)
	requestHost, _, _ := net.SplitHostPort(expected.Host)
	return isLoopback(originHost) && isLoopback(requestHost)
}
func (s *Server) expectedOrigin(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimSuffix(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func isLoopback(host string) bool {
	if host == "" {
		return false
	}
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}
func (s *Server) setCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: "ialbum_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(strings.ToLower(s.cfg.PublicURL), "https://"), SameSite: http.SameSiteLaxMode, Expires: expires})
}
func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "ialbum_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteLaxMode})
}
func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details any) {
	requestID, _ := r.Context().Value(contextKey("request-id")).(string)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message, "details": details, "requestId": requestID}})
}
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_json", "请求内容格式不正确", err.Error())
		return false
	}
	return true
}
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeError(w, r, http.StatusNotFound, "not_found", "接口不存在", nil)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	raw, err := fs.ReadFile(s.assets, name)
	if err != nil {
		name = "index.html"
		raw, err = fs.ReadFile(s.assets, name)
		w.Header().Set("Cache-Control", "no-cache")
	} else if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if err != nil {
		http.Error(w, "ialbum frontend is not built", http.StatusServiceUnavailable)
		return
	}
	if typ := mime.TypeByExtension(path.Ext(name)); typ != "" {
		w.Header().Set("Content-Type", typ)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(raw))
}
func sessionFrom(ctx context.Context) auth.Session {
	session, _ := ctx.Value(sessionKey).(auth.Session)
	return session
}
func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
