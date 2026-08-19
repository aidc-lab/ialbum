package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/skip2/go-qrcode"

	"github.com/aidc-lab/ialbum/internal/media"
	"github.com/aidc-lab/ialbum/internal/storage"
	storagemanager "github.com/aidc-lab/ialbum/internal/storage/manager"
)

func (s *Server) handleListStorages(w http.ResponseWriter, r *http.Request) {
	items, err := s.storages.List(r.Context())
	if err != nil {
		s.writeError(w, r, 500, "database_error", "无法读取存储连接", nil)
		return
	}
	s.writeJSON(w, 200, items)
}
func (s *Server) handleGetStorage(w http.ResponseWriter, r *http.Request) {
	item, err := s.storages.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	s.writeJSON(w, 200, item)
}
func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name    string              `json:"name"`
		Type    storagemanager.Type `json:"type"`
		Config  map[string]any      `json:"config"`
		Secrets map[string]string   `json:"secrets"`
	}
	if !s.decodeJSON(w, r, &input) {
		return
	}
	if input.Type == storagemanager.Baidu {
		s.writeError(w, r, 422, "use_device_flow", "百度网盘请使用设备授权流程", nil)
		return
	}
	item, err := s.storages.Create(r.Context(), storagemanager.CreateInput{Name: input.Name, Type: input.Type, Config: input.Config, Secrets: input.Secrets})
	if err != nil {
		s.writeError(w, r, 422, "storage_create_failed", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, item)
}
func (s *Server) handleUpdateStorage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name    *string           `json:"name"`
		Config  map[string]any    `json:"config"`
		Secrets map[string]string `json:"secrets"`
	}
	if !s.decodeJSON(w, r, &input) {
		return
	}
	item, err := s.storages.Update(r.Context(), chi.URLParam(r, "id"), storagemanager.UpdateInput{Name: input.Name, Config: input.Config, Secrets: input.Secrets})
	if err != nil {
		s.writeError(w, r, 422, "storage_update_failed", err.Error(), item)
		return
	}
	s.writeJSON(w, http.StatusOK, item)
}
func (s *Server) handleTestStorage(w http.ResponseWriter, r *http.Request) {
	item, err := s.storages.Test(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "storage_unavailable", err.Error(), item)
		return
	}
	s.writeJSON(w, 200, item)
}
func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request) {
	err := s.storages.Delete(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type storageBrowserEntry struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	RelativePath   string    `json:"relativePath"`
	MIMEType       string    `json:"mimeType"`
	ETag           string    `json:"etag,omitempty"`
	NativeChecksum string    `json:"nativeChecksum,omitempty"`
	Size           int64     `json:"size"`
	ModifiedAt     time.Time `json:"modifiedAt"`
	IsDir          bool      `json:"isDir"`
}

func (s *Server) handleBrowseStorage(w http.ResponseWriter, r *http.Request) {
	currentPath, err := storage.NormalizeRelative(r.URL.Query().Get("path"))
	if err != nil || len(currentPath) > 4096 {
		s.writeError(w, r, http.StatusBadRequest, "invalid_path", "目录路径无效", nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	provider, err := s.storages.Provider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	page, err := provider.List(r.Context(), currentPath, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	items := make([]storageBrowserEntry, 0, len(page.Objects))
	for _, object := range page.Objects {
		relativePath, normalizeErr := storage.NormalizeRelative(object.RelativePath)
		if normalizeErr != nil || relativePath == "" {
			continue
		}
		name := path.Base(strings.TrimSuffix(relativePath, "/"))
		if name == "." || name == "/" {
			name = relativePath
		}
		items = append(items, storageBrowserEntry{ID: object.ID, Name: name, RelativePath: relativePath, MIMEType: object.MIMEType, ETag: object.ETag, NativeChecksum: object.NativeChecksum, Size: object.Size, ModifiedAt: object.ModifiedAt, IsDir: object.IsDir})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"currentPath": currentPath, "items": items, "nextCursor": page.NextCursor})
}

func (s *Server) handleStorageObjectContent(w http.ResponseWriter, r *http.Request) {
	objectPath, err := storage.NormalizeRelative(r.URL.Query().Get("path"))
	if err != nil || objectPath == "" || len(objectPath) > 4096 {
		s.writeError(w, r, http.StatusBadRequest, "invalid_path", "文件路径无效", nil)
		return
	}
	provider, err := s.storages.Provider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	object, err := provider.Stat(r.Context(), objectPath)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	if object.IsDir {
		s.writeError(w, r, http.StatusUnprocessableEntity, "not_a_file", "目录不能作为文件打开", nil)
		return
	}
	byteRange, err := parseRange(r.Header.Get("Range"), object.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", object.Size))
		s.writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "请求范围无效", nil)
		return
	}
	reader, opened, err := provider.Open(r.Context(), objectPath, byteRange)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	defer reader.Close()
	mimeType := object.MIMEType
	if mimeType == "" {
		mimeType = storage.MIMEFromName(objectPath)
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, no-store")
	if provider.Capabilities().Range {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if object.ETag != "" {
		w.Header().Set("ETag", `"`+strings.Trim(object.ETag, `"`)+`"`)
	}
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", contentDisposition(disposition, path.Base(objectPath)))
	length := opened.Size
	if byteRange != nil && opened.RangeApplied {
		length = byteRange.Length
		if length <= 0 || byteRange.Start+length > opened.Size {
			length = opened.Size - byteRange.Start
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.Start, byteRange.Start+length-1, opened.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.CopyBuffer(w, reader, make([]byte, 64*1024))
}
func (s *Server) handleStartBaiduFlow(w http.ResponseWriter, r *http.Request) {
	var input struct{ Name, AppKey, SecretKey, Root string }
	if !s.decodeJSON(w, r, &input) {
		return
	}
	flow, err := s.storages.StartBaiduDeviceFlow(r.Context(), input.Name, input.AppKey, input.SecretKey, input.Root)
	if err != nil {
		s.writeError(w, r, 422, "baidu_authorization_failed", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, flow)
}
func (s *Server) handleGetBaiduFlow(w http.ResponseWriter, r *http.Request) {
	flow, err := s.storages.GetBaiduDeviceFlow(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	s.writeJSON(w, 200, flow)
}
func (s *Server) handleCancelBaiduFlow(w http.ResponseWriter, r *http.Request) {
	if err := s.storages.CancelBaiduDeviceFlow(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleBaiduFlowQR(w http.ResponseWriter, r *http.Request) {
	target, err := s.storages.GetQRCode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	png, err := qrcode.Encode(target, qrcode.Medium, 256)
	if err != nil {
		s.writeError(w, r, 500, "qr_failed", "无法生成二维码", nil)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) handleListAlbums(w http.ResponseWriter, r *http.Request) {
	items, err := s.catalog.ListAlbums(r.Context(), r.URL.Query().Get("tag"))
	if err != nil {
		s.writeError(w, r, 500, "database_error", "无法读取相册", nil)
		return
	}
	s.writeJSON(w, 200, items)
}
func (s *Server) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
	album, err := s.catalog.GetAlbum(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	s.writeJSON(w, 200, album)
}
func (s *Server) handleCreateAlbum(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name, Description, PrimaryStorageID, PrimaryPath, BackupStorageID, BackupPath, BackupMode string
		Tags                                                                                      []string
		BackupEnabled, SyncOnUpload                                                               *bool
		ScanIntervalSeconds                                                                       *int64
	}
	if !s.decodeJSON(w, r, &input) {
		return
	}
	backupEnabled := true
	syncOnUpload := true
	if input.BackupEnabled != nil {
		backupEnabled = *input.BackupEnabled
	}
	if input.SyncOnUpload != nil {
		syncOnUpload = *input.SyncOnUpload
	}
	if input.ScanIntervalSeconds == nil {
		value := int64(s.cfg.DefaultScanInterval.Seconds())
		input.ScanIntervalSeconds = &value
	}
	album, err := s.catalog.CreateAlbum(r.Context(), media.CreateAlbumInput{Name: input.Name, Description: input.Description, PrimaryStorageID: input.PrimaryStorageID, PrimaryPath: input.PrimaryPath, BackupStorageID: input.BackupStorageID, BackupPath: input.BackupPath, BackupMode: input.BackupMode, Tags: input.Tags, BackupEnabled: backupEnabled, SyncOnUpload: syncOnUpload, ScanIntervalSeconds: input.ScanIntervalSeconds})
	if err != nil {
		s.writeError(w, r, 422, "album_create_failed", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, album)
}
func (s *Server) handleUpdateAlbum(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name, Description           *string
		BackupMode                  string
		Tags                        []string
		BackupEnabled, SyncOnUpload *bool
		ScanIntervalSeconds         *int64
		ClearScanInterval           bool
	}
	if !s.decodeJSON(w, r, &input) {
		return
	}
	album, err := s.catalog.UpdateAlbum(r.Context(), chi.URLParam(r, "id"), media.UpdateAlbumInput{Name: input.Name, Description: input.Description, BackupMode: input.BackupMode, Tags: input.Tags, BackupEnabled: input.BackupEnabled, SyncOnUpload: input.SyncOnUpload, ScanIntervalSeconds: input.ScanIntervalSeconds, ClearScanInterval: input.ClearScanInterval})
	if err != nil {
		s.writeError(w, r, 422, "album_update_failed", err.Error(), nil)
		return
	}
	s.writeJSON(w, 200, album)
}
func (s *Server) handleDeleteAlbum(w http.ResponseWriter, r *http.Request) {
	if err := s.catalog.RemoveAlbum(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleScanAlbum(w http.ResponseWriter, r *http.Request) {
	jobID, err := s.catalog.EnqueueScan(r.Context(), chi.URLParam(r, "id"), true)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"jobId": jobID})
}
func (s *Server) handleSetCover(w http.ResponseWriter, r *http.Request) {
	var input struct {
		MediaID string `json:"mediaId"`
	}
	if !s.decodeJSON(w, r, &input) {
		return
	}
	if err := s.catalog.SetCover(r.Context(), chi.URLParam(r, "id"), input.MediaID); err != nil {
		s.writeError(w, r, 422, "cover_update_failed", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, next, err := s.catalog.ListMedia(r.Context(), chi.URLParam(r, "id"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	s.writeJSON(w, 200, map[string]any{"items": items, "nextCursor": next})
}
func (s *Server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, 200, map[string]any{"listen": s.cfg.Listen, "dataDir": s.cfg.DataDir, "maxUploadBytes": s.cfg.MaxUploadBytes, "cacheMaxBytes": s.cfg.CacheMaxBytes, "maxVideoStagingBytes": s.cfg.MaxVideoStagingBytes, "ffmpegAvailable": s.processor.Available(), "defaultScanIntervalSeconds": int64(s.cfg.DefaultScanInterval.Seconds()), "serverTime": time.Now().UTC()})
}

func (s *Server) resourceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, storage.ErrNotFound):
		s.writeError(w, r, 404, "not_found", "资源不存在", nil)
	case errors.Is(err, storage.ErrConflict):
		s.writeError(w, r, 409, "conflict", "资源正在使用或发生冲突", nil)
	case errors.Is(err, storage.ErrUnauthorized):
		s.writeError(w, r, 503, "storage_unauthorized", "存储需要重新授权", nil)
	default:
		s.writeError(w, r, 500, "internal_error", err.Error(), nil)
	}
}
func boolQuery(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "true" || value == "yes"
}
