package api

import (
	"archive/zip"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aidc-lab/ialbum/internal/media"
	"github.com/aidc-lab/ialbum/internal/storage"
)

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "id")
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		s.writeError(w, r, 422, "idempotency_key_required", "上传必须提供有效的 Idempotency-Key", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		s.writeError(w, r, 400, "invalid_multipart", "上传格式无效", nil)
		return
	}
	var filename string
	tmp, err := os.CreateTemp(s.cfg.TempDir, "upload-*")
	if err != nil {
		s.writeError(w, r, 500, "temp_file_failed", "无法准备上传空间", nil)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	var written int64
	found := false
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			tmp.Close()
			s.writeError(w, r, 400, "upload_failed", nextErr.Error(), nil)
			return
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		found = true
		filename = path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		written, err = copyLimited(tmp, part, s.cfg.MaxUploadBytes)
		part.Close()
		break
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if !found || filename == "" {
		s.writeError(w, r, 422, "file_required", "请选择一个文件", nil)
		return
	}
	if err != nil {
		status := 500
		if errors.Is(err, errTooLarge) {
			status = 413
		}
		s.writeError(w, r, status, "upload_failed", err.Error(), nil)
		return
	}
	item, err := s.catalog.Upload(r.Context(), albumID, filename, tmpPath, key, written)
	if err != nil {
		if errors.Is(err, media.ErrUnsupportedMedia) {
			s.writeError(w, r, 415, "unsupported_media", "不支持这种媒体格式", nil)
		} else {
			s.writeError(w, r, 422, "upload_failed", err.Error(), nil)
		}
		return
	}
	s.writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleMediaContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	item, err := s.catalog.GetMedia(r.Context(), id)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	br, err := parseRange(r.Header.Get("Range"), item.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", item.Size))
		s.writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "请求范围无效", nil)
		return
	}
	reader, obj, _, err := s.catalog.OpenMedia(r.Context(), id, br)
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", item.MIMEType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+item.SourceVersion+`"`)
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", contentDisposition(disposition, path.Base(item.RelativePath)))
	length := obj.Size
	if br != nil && obj.RangeApplied {
		length = br.Length
		if length <= 0 || br.Start+length > obj.Size {
			length = obj.Size - br.Start
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", br.Start, br.Start+length-1, obj.Size))
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
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	file, version, err := s.processor.OpenThumbnail(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "variant"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "thumbnail_pending", "缩略图尚未生成", nil)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+version+`"`)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) handleCreateDownloadTicket(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AlbumID  string   `json:"albumId"`
		MediaIDs []string `json:"mediaIds"`
	}
	if !s.decodeJSON(w, r, &input) {
		return
	}
	var albumName string
	if input.AlbumID != "" {
		album, err := s.catalog.GetAlbum(r.Context(), input.AlbumID)
		if err != nil {
			s.resourceError(w, r, err)
			return
		}
		albumName = album.Name
	}
	if len(input.MediaIDs) == 0 {
		if input.AlbumID == "" {
			s.writeError(w, r, 422, "selection_required", "请选择相册或媒体", nil)
			return
		}
		cursor := ""
		for {
			items, next, err := s.catalog.ListMedia(r.Context(), input.AlbumID, cursor, 200)
			if err != nil {
				s.resourceError(w, r, err)
				return
			}
			for _, item := range items {
				input.MediaIDs = append(input.MediaIDs, item.ID)
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	if len(input.MediaIDs) == 0 {
		s.writeError(w, r, 422, "empty_download", "没有可下载的媒体", nil)
		return
	}
	if len(input.MediaIDs) > 100000 {
		s.writeError(w, r, 422, "too_many_items", "单次下载媒体数量过多", nil)
		return
	}
	for _, id := range input.MediaIDs {
		item, err := s.catalog.GetMedia(r.Context(), id)
		if err != nil {
			s.resourceError(w, r, err)
			return
		}
		if input.AlbumID != "" && item.AlbumID != input.AlbumID {
			s.writeError(w, r, 422, "invalid_selection", "所选媒体不属于该相册", nil)
			return
		}
	}
	token, err := randomToken(32)
	if err != nil {
		s.writeError(w, r, 500, "token_failed", "无法创建下载任务", nil)
		return
	}
	hash := sha256.Sum256([]byte(token))
	raw, _ := json.Marshal(input.MediaIDs)
	session := sessionFrom(r.Context())
	filename := sanitizeFilename(albumName)
	if filename == "" {
		filename = "ialbum-selection"
	}
	id, err := randomToken(18)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	expires := now.Add(5 * time.Minute)
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO download_tickets(id,token_hash,session_id,album_id,media_ids_json,filename,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, hash[:], session.ID, nullString(input.AlbumID), string(raw), filename+".zip", expires.Unix(), now.Unix())
	if err != nil {
		s.writeError(w, r, 500, "ticket_failed", "无法创建下载任务", nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"url": "/api/v1/downloads/" + token, "expiresAt": expires})
}
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.downloadSlots <- struct{}{}:
		defer func() { <-s.downloadSlots }()
	default:
		s.writeError(w, r, 429, "download_busy", "当前打包任务过多，请稍后重试", nil)
		return
	}
	tokenHash := sha256.Sum256([]byte(chi.URLParam(r, "token")))
	session := sessionFrom(r.Context())
	var ticketID, rawIDs, filename string
	var expires int64
	err := s.db.QueryRowContext(r.Context(), `SELECT id,media_ids_json,filename,expires_at FROM download_tickets WHERE token_hash=? AND session_id=? AND consumed_at IS NULL`, tokenHash[:], session.ID).Scan(&ticketID, &rawIDs, &filename, &expires)
	if err != nil || time.Now().Unix() > expires {
		s.writeError(w, r, 404, "download_ticket_invalid", "下载链接已失效", nil)
		return
	}
	result, err := s.db.ExecContext(r.Context(), `UPDATE download_tickets SET consumed_at=? WHERE id=? AND consumed_at IS NULL`, time.Now().Unix(), ticketID)
	if err != nil {
		s.writeError(w, r, 500, "download_failed", "无法开始下载", nil)
		return
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		s.writeError(w, r, 404, "download_ticket_invalid", "下载链接已使用", nil)
		return
	}
	var ids []string
	if err := json.Unmarshal([]byte(rawIDs), &ids); err != nil {
		s.writeError(w, r, 500, "download_failed", "下载清单损坏", nil)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", filename))
	w.Header().Set("Cache-Control", "no-store")
	archive := zip.NewWriter(w)
	used := map[string]int{}
	var failures []string
	for _, id := range ids {
		item, err := s.catalog.GetMedia(r.Context(), id)
		if err != nil {
			failures = append(failures, id+": "+err.Error())
			continue
		}
		reader, _, _, err := s.catalog.OpenMedia(r.Context(), id, nil)
		if err != nil {
			failures = append(failures, item.RelativePath+": "+err.Error())
			continue
		}
		entryName := safeZipName(item.RelativePath, used)
		header := &zip.FileHeader{Name: entryName, Method: zip.Store}
		header.SetModTime(item.ModifiedAt)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			reader.Close()
			failures = append(failures, entryName+": "+err.Error())
			continue
		}
		_, copyErr := io.CopyBuffer(writer, reader, make([]byte, 64*1024))
		reader.Close()
		if copyErr != nil {
			failures = append(failures, entryName+": "+copyErr.Error())
		}
	}
	if len(failures) > 0 {
		if writer, err := archive.Create("ialbum-download-errors.txt"); err == nil {
			_, _ = writer.Write([]byte(strings.Join(failures, "\n") + "\n"))
		}
	}
	_ = archive.Close()
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	items, err := s.jobs.List(r.Context(), r.URL.Query().Get("state"), 100)
	if err != nil {
		s.writeError(w, r, 500, "database_error", "无法读取任务", nil)
		return
	}
	s.writeJSON(w, 200, items)
}
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Retry(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleCancelDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.catalog.CancelMirrorDelete(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleResumeDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.catalog.ResumeMirrorDelete(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.resourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errTooLarge = errors.New("file exceeds upload limit")

func copyLimited(dst io.Writer, src io.Reader, max int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, max+1))
	if err != nil {
		return written, err
	}
	if written > max {
		return written, errTooLarge
	}
	return written, nil
}
func parseRange(value string, size int64) (*storage.ByteRange, error) {
	if value == "" {
		return nil, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return nil, errors.New("unsupported range")
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return nil, errors.New("invalid range")
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return nil, errors.New("invalid range")
	}
	length := size - start
	if parts[1] != "" {
		end, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return nil, errors.New("invalid range")
		}
		if end >= size {
			end = size - 1
		}
		length = end - start + 1
	}
	return &storage.ByteRange{Start: start, Length: length}, nil
}
func contentDisposition(kind, filename string) string {
	safe := strings.NewReplacer("\"", "_", "\r", "_", "\n", "_").Replace(filename)
	encoded := strings.ReplaceAll(url.PathEscape(filename), "+", "%20")
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, safe, encoded)
}
func sanitizeFilename(value string) string {
	return strings.Trim(strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(value), " .")
}
func safeZipName(value string, used map[string]int) string {
	name := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(value, "\\", "/")), "/")
	if name == "" || name == "." {
		name = "media"
	}
	key := strings.ToLower(name)
	if count := used[key]; count > 0 {
		ext := path.Ext(name)
		base := strings.TrimSuffix(name, ext)
		name = fmt.Sprintf("%s (%d)%s", base, count, ext)
	}
	used[key]++
	return name
}
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ = sql.ErrNoRows
