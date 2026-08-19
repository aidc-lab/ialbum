package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	appdb "github.com/aidc-lab/ialbum/internal/db"
	"github.com/aidc-lab/ialbum/internal/jobs"
	"github.com/aidc-lab/ialbum/internal/storage"
	storagemanager "github.com/aidc-lab/ialbum/internal/storage/manager"
)

var supportedImages = map[string]string{".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".gif": "image/gif"}
var supportedVideos = map[string]string{".mp4": "video/mp4", ".m4v": "video/x-m4v", ".mov": "video/quicktime", ".webm": "video/webm", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo"}

type Album struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Description         string     `json:"description"`
	BackupMode          string     `json:"backupMode"`
	ScanStatus          string     `json:"scanStatus"`
	ScanError           string     `json:"scanError,omitempty"`
	CoverMediaID        *string    `json:"coverMediaId,omitempty"`
	BackupEnabled       bool       `json:"backupEnabled"`
	SyncOnUpload        bool       `json:"syncOnUpload"`
	ScanIntervalSeconds *int64     `json:"scanIntervalSeconds,omitempty"`
	LastScanAt          *time.Time `json:"lastScanAt,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
	Tags                []string   `json:"tags"`
	MediaCount          int        `json:"mediaCount"`
	Primary             *Binding   `json:"primary"`
	Backup              *Binding   `json:"backup,omitempty"`
}
type Binding struct {
	ID        string `json:"id"`
	StorageID string `json:"storageId"`
	Role      string `json:"role"`
	RootPath  string `json:"rootPath"`
}
type Media struct {
	ID               string     `json:"id"`
	AlbumID          string     `json:"albumId"`
	RelativePath     string     `json:"relativePath"`
	Kind             string     `json:"kind"`
	MIMEType         string     `json:"mimeType"`
	SourceVersion    string     `json:"sourceVersion"`
	ProcessingStatus string     `json:"processingStatus"`
	Size             int64      `json:"size"`
	ModifiedAt       time.Time  `json:"modifiedAt"`
	Width            *int64     `json:"width,omitempty"`
	Height           *int64     `json:"height,omitempty"`
	DurationMS       *int64     `json:"durationMs,omitempty"`
	TakenAt          *time.Time `json:"takenAt,omitempty"`
	Missing          bool       `json:"missing"`
}
type CreateAlbumInput struct {
	Name, Description, PrimaryStorageID, PrimaryPath, BackupStorageID, BackupPath, BackupMode string
	Tags                                                                                      []string
	BackupEnabled, SyncOnUpload                                                               bool
	ScanIntervalSeconds                                                                       *int64
}
type UpdateAlbumInput struct {
	Name, Description           *string
	BackupMode                  string
	Tags                        []string
	BackupEnabled, SyncOnUpload *bool
	ScanIntervalSeconds         *int64
	ClearScanInterval           bool
}
type Service struct {
	db                *appdb.DB
	storages          *storagemanager.Manager
	queue             *jobs.Queue
	tempDir, cacheDir string
	mirrorGrace       time.Duration
}

func NewService(db *appdb.DB, storages *storagemanager.Manager, queue *jobs.Queue, tempDir, cacheDir string, mirrorGrace time.Duration) *Service {
	return &Service{db: db, storages: storages, queue: queue, tempDir: tempDir, cacheDir: cacheDir, mirrorGrace: mirrorGrace}
}
func (s *Service) CreateAlbum(ctx context.Context, in CreateAlbumInput) (Album, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		return Album{}, errors.New("album name is required and must not exceed 120 characters")
	}
	if len(in.Description) > 2000 {
		return Album{}, errors.New("description must not exceed 2000 characters")
	}
	primaryPath, err := storage.NormalizeRelative(in.PrimaryPath)
	if err != nil {
		return Album{}, err
	}
	backupPath, err := storage.NormalizeRelative(in.BackupPath)
	if err != nil {
		return Album{}, err
	}
	if in.PrimaryStorageID == "" {
		return Album{}, errors.New("primary storage is required")
	}
	if in.BackupMode == "" {
		in.BackupMode = "safe"
	}
	if in.BackupMode != "safe" && in.BackupMode != "mirror" {
		return Album{}, errors.New("invalid backup mode")
	}
	if in.ScanIntervalSeconds != nil && (*in.ScanIntervalSeconds < 900 || *in.ScanIntervalSeconds > 604800) {
		return Album{}, errors.New("scan interval must be between 15 minutes and 7 days")
	}
	if err := s.ensureNoOverlap(ctx, in.PrimaryStorageID, primaryPath, ""); err != nil {
		return Album{}, err
	}
	primary, err := s.storages.Provider(ctx, in.PrimaryStorageID)
	if err != nil {
		return Album{}, err
	}
	if err := primary.Mkdir(ctx, primaryPath); err != nil {
		return Album{}, fmt.Errorf("prepare primary directory: %w", err)
	}
	id := newID()
	if in.BackupStorageID != "" {
		if in.BackupStorageID == in.PrimaryStorageID && pathsOverlap(primaryPath, backupPath) {
			return Album{}, errors.New("primary and backup paths overlap")
		}
		if err := s.ensureNoOverlap(ctx, in.BackupStorageID, backupPath, ""); err != nil {
			return Album{}, err
		}
		backup, err := s.storages.Provider(ctx, in.BackupStorageID)
		if err != nil {
			return Album{}, err
		}
		if err := prepareBackupRoot(ctx, backup, backupPath, id); err != nil {
			return Album{}, err
		}
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Album{}, err
	}
	var interval any
	if in.ScanIntervalSeconds != nil {
		interval = *in.ScanIntervalSeconds
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO albums(id,name,description,backup_enabled,backup_mode,sync_on_upload,scan_interval_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, in.Name, in.Description, boolInt(in.BackupEnabled), in.BackupMode, boolInt(in.SyncOnUpload), interval, now.Unix(), now.Unix())
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO album_storage_bindings(id,album_id,storage_id,role,root_path,created_at) VALUES(?,?,?,?,?,?)`, newID(), id, in.PrimaryStorageID, "primary", primaryPath, now.Unix())
	}
	if err == nil && in.BackupStorageID != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO album_storage_bindings(id,album_id,storage_id,role,root_path,created_at) VALUES(?,?,?,?,?,?)`, newID(), id, in.BackupStorageID, "backup", backupPath, now.Unix())
	}
	if err == nil {
		err = s.setTagsTx(ctx, tx, id, in.Tags)
	}
	if err != nil {
		_ = tx.Rollback()
		return Album{}, err
	}
	if err := tx.Commit(); err != nil {
		return Album{}, err
	}
	_, _ = s.EnqueueScan(ctx, id, true)
	return s.GetAlbum(ctx, id)
}
func (s *Service) ListAlbums(ctx context.Context, tag string) ([]Album, error) {
	query := `SELECT a.id FROM albums a WHERE a.removed_at IS NULL`
	args := []any{}
	if tag != "" {
		query += ` AND EXISTS(SELECT 1 FROM album_tags at JOIN tags t ON t.id=at.tag_id WHERE at.album_id=a.id AND t.normalized_name=?)`
		args = append(args, normalizeTag(tag))
	}
	query += ` ORDER BY a.created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Album, 0)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		album, err := s.GetAlbum(ctx, id)
		if err == nil {
			result = append(result, album)
		}
	}
	return result, rows.Err()
}
func (s *Service) GetAlbum(ctx context.Context, id string) (Album, error) {
	var a Album
	var cover sql.NullString
	var interval, lastScan sql.NullInt64
	var backupEnabled, sync int
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,description,cover_media_id,backup_enabled,backup_mode,sync_on_upload,scan_interval_seconds,last_scan_at,scan_status,scan_error,created_at,updated_at FROM albums WHERE id=? AND removed_at IS NULL`, id).Scan(&a.ID, &a.Name, &a.Description, &cover, &backupEnabled, &a.BackupMode, &sync, &interval, &lastScan, &a.ScanStatus, &a.ScanError, &created, &updated)
	if err != nil {
		return a, err
	}
	if cover.Valid {
		a.CoverMediaID = &cover.String
	}
	a.BackupEnabled = backupEnabled == 1
	a.SyncOnUpload = sync == 1
	if interval.Valid {
		v := interval.Int64
		a.ScanIntervalSeconds = &v
	}
	if lastScan.Valid {
		v := time.Unix(lastScan.Int64, 0).UTC()
		a.LastScanAt = &v
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	a.UpdatedAt = time.Unix(updated, 0).UTC()
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_items WHERE album_id=? AND missing_scans=0`, id).Scan(&a.MediaCount)
	tagRows, _ := s.db.QueryContext(ctx, `SELECT t.name FROM tags t JOIN album_tags at ON at.tag_id=t.id WHERE at.album_id=? ORDER BY t.normalized_name`, id)
	if tagRows != nil {
		defer tagRows.Close()
		for tagRows.Next() {
			var value string
			_ = tagRows.Scan(&value)
			a.Tags = append(a.Tags, value)
		}
	}
	bindingRows, _ := s.db.QueryContext(ctx, `SELECT id,storage_id,role,root_path FROM album_storage_bindings WHERE album_id=?`, id)
	if bindingRows != nil {
		defer bindingRows.Close()
		for bindingRows.Next() {
			b := &Binding{}
			_ = bindingRows.Scan(&b.ID, &b.StorageID, &b.Role, &b.RootPath)
			if b.Role == "primary" {
				a.Primary = b
			} else {
				a.Backup = b
			}
		}
	}
	return a, nil
}
func (s *Service) UpdateAlbum(ctx context.Context, id string, in UpdateAlbumInput) (Album, error) {
	album, err := s.GetAlbum(ctx, id)
	if err != nil {
		return Album{}, err
	}
	if in.Name != nil {
		album.Name = strings.TrimSpace(*in.Name)
		if album.Name == "" || len(album.Name) > 120 {
			return Album{}, errors.New("album name is required and must not exceed 120 characters")
		}
	}
	if in.Description != nil {
		if len(*in.Description) > 2000 {
			return Album{}, errors.New("description must not exceed 2000 characters")
		}
		album.Description = *in.Description
	}
	if in.BackupMode != "" {
		if in.BackupMode != "safe" && in.BackupMode != "mirror" {
			return Album{}, errors.New("invalid backup mode")
		}
		album.BackupMode = in.BackupMode
	}
	if in.BackupEnabled != nil {
		album.BackupEnabled = *in.BackupEnabled
	}
	if in.SyncOnUpload != nil {
		album.SyncOnUpload = *in.SyncOnUpload
	}
	if in.ClearScanInterval {
		album.ScanIntervalSeconds = nil
	} else if in.ScanIntervalSeconds != nil {
		if *in.ScanIntervalSeconds < 900 || *in.ScanIntervalSeconds > 604800 {
			return Album{}, errors.New("invalid scan interval")
		}
		album.ScanIntervalSeconds = in.ScanIntervalSeconds
	}
	var interval any
	if album.ScanIntervalSeconds != nil {
		interval = *album.ScanIntervalSeconds
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Album{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE albums SET name=?,description=?,backup_enabled=?,backup_mode=?,sync_on_upload=?,scan_interval_seconds=?,updated_at=? WHERE id=? AND removed_at IS NULL`, album.Name, album.Description, boolInt(album.BackupEnabled), album.BackupMode, boolInt(album.SyncOnUpload), interval, time.Now().Unix(), id)
	if err == nil && in.Tags != nil {
		err = s.setTagsTx(ctx, tx, id, in.Tags)
	}
	if err != nil {
		_ = tx.Rollback()
		return Album{}, err
	}
	if err = tx.Commit(); err != nil {
		return Album{}, err
	}
	if album.BackupMode == "safe" {
		_, _ = s.db.ExecContext(ctx, `UPDATE media_replicas SET delete_after=NULL,status=CASE WHEN status='pending-delete' THEN 'ready' ELSE status END WHERE media_id IN (SELECT id FROM media_items WHERE album_id=?)`, id)
	}
	return s.GetAlbum(ctx, id)
}
func (s *Service) SetCover(ctx context.Context, albumID, mediaID string) error {
	if mediaID == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE albums SET cover_media_id=NULL,updated_at=? WHERE id=?`, time.Now().Unix(), albumID)
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_items WHERE id=? AND album_id=? AND kind='image' AND missing_scans=0`, mediaID, albumID).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return errors.New("cover must be an available image from this album")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE albums SET cover_media_id=?,updated_at=? WHERE id=?`, mediaID, time.Now().Unix(), albumID)
	return err
}
func (s *Service) RemoveAlbum(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE albums SET removed_at=?,generation=generation+1,updated_at=? WHERE id=? AND removed_at IS NULL`, time.Now().Unix(), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	_ = s.queue.CancelAlbum(ctx, id)
	_, _ = s.db.ExecContext(ctx, `UPDATE media_replicas SET status='cancelled',delete_after=NULL WHERE media_id IN (SELECT id FROM media_items WHERE album_id=?)`, id)
	return nil
}
func (s *Service) ListMedia(ctx context.Context, albumID, cursor string, limit int) ([]Media, string, error) {
	offset, _ := strconv.Atoi(cursor)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,album_id,relative_path,kind,mime_type,size,modified_at,width,height,duration_ms,taken_at,source_version,processing_status,missing_scans FROM media_items WHERE album_id=? AND missing_scans=0 ORDER BY COALESCE(taken_at,modified_at) DESC,id LIMIT ? OFFSET ?`, albumID, limit+1, offset)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	result := make([]Media, 0)
	for rows.Next() {
		var m Media
		var width, height, duration, taken sql.NullInt64
		var modified int64
		var missing int
		if err := rows.Scan(&m.ID, &m.AlbumID, &m.RelativePath, &m.Kind, &m.MIMEType, &m.Size, &modified, &width, &height, &duration, &taken, &m.SourceVersion, &m.ProcessingStatus, &missing); err != nil {
			return nil, "", err
		}
		m.ModifiedAt = time.Unix(modified, 0).UTC()
		m.Width = nullInt(width)
		m.Height = nullInt(height)
		m.DurationMS = nullInt(duration)
		if taken.Valid {
			v := time.Unix(taken.Int64, 0).UTC()
			m.TakenAt = &v
		}
		m.Missing = missing > 0
		result = append(result, m)
	}
	next := ""
	if len(result) > limit {
		result = result[:limit]
		next = strconv.Itoa(offset + limit)
	}
	return result, next, rows.Err()
}
func (s *Service) GetMedia(ctx context.Context, id string) (Media, error) {
	var m Media
	var width, height, duration, taken sql.NullInt64
	var modified int64
	var missing int
	err := s.db.QueryRowContext(ctx, `SELECT id,album_id,relative_path,kind,mime_type,size,modified_at,width,height,duration_ms,taken_at,source_version,processing_status,missing_scans FROM media_items WHERE id=?`, id).Scan(&m.ID, &m.AlbumID, &m.RelativePath, &m.Kind, &m.MIMEType, &m.Size, &modified, &width, &height, &duration, &taken, &m.SourceVersion, &m.ProcessingStatus, &missing)
	m.ModifiedAt = time.Unix(modified, 0).UTC()
	m.Width = nullInt(width)
	m.Height = nullInt(height)
	m.DurationMS = nullInt(duration)
	if taken.Valid {
		v := time.Unix(taken.Int64, 0).UTC()
		m.TakenAt = &v
	}
	m.Missing = missing > 0
	return m, err
}
func (s *Service) OpenMedia(ctx context.Context, id string, br *storage.ByteRange) (io.ReadCloser, storage.Object, Media, error) {
	m, err := s.GetMedia(ctx, id)
	if err != nil {
		return nil, storage.Object{}, m, err
	}
	album, err := s.GetAlbum(ctx, m.AlbumID)
	if err != nil {
		return nil, storage.Object{}, m, err
	}
	provider, err := s.storages.Provider(ctx, album.Primary.StorageID)
	if err != nil {
		return nil, storage.Object{}, m, err
	}
	reader, obj, err := provider.Open(ctx, joinPath(album.Primary.RootPath, m.RelativePath), br)
	return reader, obj, m, err
}
func (s *Service) EnqueueScan(ctx context.Context, albumID string, immediate bool) (string, error) {
	var generation int
	if err := s.db.QueryRowContext(ctx, `SELECT generation FROM albums WHERE id=? AND removed_at IS NULL`, albumID).Scan(&generation); err != nil {
		return "", err
	}
	runAt := time.Now()
	if !immediate {
		runAt = runAt.Add(time.Minute)
	}
	bucket := runAt.Unix() / 60
	return s.queue.Enqueue(ctx, "scan", fmt.Sprintf("scan:%s:%d:%d", albumID, generation, bucket), map[string]any{"albumId": albumID, "generation": generation}, runAt)
}
func (s *Service) ScheduleDueScans(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,scan_interval_seconds,last_scan_at FROM albums WHERE removed_at IS NULL AND scan_interval_seconds IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var id string
		var interval int64
		var last sql.NullInt64
		if err := rows.Scan(&id, &interval, &last); err != nil {
			return err
		}
		if !last.Valid || time.Unix(last.Int64, 0).Add(time.Duration(interval)*time.Second).Before(now) {
			_, _ = s.EnqueueScan(ctx, id, true)
		}
	}
	return rows.Err()
}
func (s *Service) ScanJob(ctx context.Context, job jobs.Job) error {
	var payload struct {
		AlbumID    string `json:"albumId"`
		Generation int    `json:"generation"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	return s.Scan(ctx, payload.AlbumID, payload.Generation)
}
func (s *Service) Scan(ctx context.Context, albumID string, generation int) error {
	album, err := s.GetAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	var currentGeneration int
	if err := s.db.QueryRowContext(ctx, `SELECT generation FROM albums WHERE id=?`, albumID).Scan(&currentGeneration); err != nil {
		return err
	}
	if currentGeneration != generation {
		return nil
	}
	provider, err := s.storages.Provider(ctx, album.Primary.StorageID)
	if err != nil {
		return err
	}
	scanID := newID()
	now := time.Now().UTC()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO scan_runs(id,album_id,status,started_at) VALUES(?,?,?,?)`, scanID, albumID, "running", now.Unix())
	_, _ = s.db.ExecContext(ctx, `UPDATE albums SET scan_status='running',scan_error='' WHERE id=?`, albumID)
	objects, err := walkProvider(ctx, provider, album.Primary.RootPath)
	if err != nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE scan_runs SET status='failed',error_message=?,completed_at=? WHERE id=?`, err.Error(), time.Now().Unix(), scanID)
		_, _ = s.db.ExecContext(ctx, `UPDATE albums SET scan_status='failed',scan_error=?,updated_at=? WHERE id=?`, err.Error(), time.Now().Unix(), albumID)
		return err
	}
	seen := map[string]bool{}
	discovered := 0
	for _, obj := range objects {
		kind, mime, ok := classify(obj.RelativePath)
		if !ok {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(obj.RelativePath, album.Primary.RootPath), "/")
		if rel == "" {
			continue
		}
		seen[normalizePath(rel)] = true
		changed, mediaID, err := s.upsertObject(ctx, albumID, rel, kind, mime, obj)
		if err != nil {
			return err
		}
		discovered++
		if changed {
			_, _ = s.queue.Enqueue(ctx, "thumbnail", "thumb:"+mediaID+":"+fingerprint(obj), map[string]any{"albumId": albumID, "mediaId": mediaID}, time.Now())
			if album.Backup != nil && album.BackupEnabled {
				_, _ = s.queue.Enqueue(ctx, "backup", "backup:"+mediaID+":"+fingerprint(obj), map[string]any{"albumId": albumID, "mediaId": mediaID}, time.Now())
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,normalized_path,missing_scans,first_missing_at FROM media_items WHERE album_id=?`, albumID)
	if err != nil {
		return err
	}
	type missingRow struct {
		id, path string
		count    int
		first    sql.NullInt64
	}
	var missing []missingRow
	for rows.Next() {
		var row missingRow
		_ = rows.Scan(&row.id, &row.path, &row.count, &row.first)
		if !seen[row.path] {
			missing = append(missing, row)
		} else {
			_, _ = s.db.ExecContext(ctx, `UPDATE media_items SET missing_scans=0,first_missing_at=NULL,delete_suppressed=0,updated_at=? WHERE id=?`, time.Now().Unix(), row.id)
		}
	}
	rows.Close()
	for _, row := range missing {
		first := time.Now().UTC()
		if row.first.Valid {
			first = time.Unix(row.first.Int64, 0).UTC()
		}
		newCount := row.count + 1
		_, _ = s.db.ExecContext(ctx, `UPDATE media_items SET missing_scans=?,first_missing_at=COALESCE(first_missing_at,?),updated_at=? WHERE id=?`, newCount, first.Unix(), time.Now().Unix(), row.id)
		if album.BackupMode == "mirror" && album.Backup != nil && newCount >= 2 {
			s.scheduleMirrorDelete(ctx, album, row.id, first)
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE albums SET cover_media_id=NULL WHERE id=? AND cover_media_id IN (SELECT id FROM media_items WHERE album_id=? AND missing_scans>0)`, albumID, albumID)
	completed := time.Now().UTC()
	_, _ = s.db.ExecContext(ctx, `UPDATE scan_runs SET status='succeeded',discovered_count=?,missing_count=?,completed_at=? WHERE id=?`, discovered, len(missing), completed.Unix(), scanID)
	_, _ = s.db.ExecContext(ctx, `UPDATE albums SET scan_status='ready',scan_error='',last_scan_at=?,updated_at=? WHERE id=?`, completed.Unix(), completed.Unix(), albumID)
	return nil
}
func (s *Service) Upload(ctx context.Context, albumID, filename, tempPath, idempotencyKey string, size int64) (Media, error) {
	var existing sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT media_id FROM upload_requests WHERE album_id=? AND idempotency_key=? AND status='succeeded'`, albumID, idempotencyKey).Scan(&existing)
	if err == nil && existing.Valid {
		return s.GetMedia(ctx, existing.String)
	}
	if err != nil && err != sql.ErrNoRows {
		return Media{}, err
	}
	kind, mime, ok := classify(filename)
	if !ok {
		return Media{}, ErrUnsupportedMedia
	}
	album, err := s.GetAlbum(ctx, albumID)
	if err != nil {
		return Media{}, err
	}
	provider, err := s.storages.Provider(ctx, album.Primary.StorageID)
	if err != nil {
		return Media{}, err
	}
	target, err := availableName(ctx, provider, album.Primary.RootPath, path.Base(filename))
	if err != nil {
		return Media{}, err
	}
	uploadID := newID()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO upload_requests(id,album_id,idempotency_key,original_filename,target_path,temp_path,byte_size,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(album_id,idempotency_key) DO UPDATE SET updated_at=excluded.updated_at`, uploadID, albumID, idempotencyKey, filename, target, tempPath, size, "uploading", now.Unix(), now.Unix())
	if err != nil {
		return Media{}, err
	}
	obj, err := provider.Put(ctx, joinPath(album.Primary.RootPath, target), storage.FileSource{Path: tempPath, ByteSize: size}, storage.PutOptions{Conflict: "fail"})
	if err != nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE upload_requests SET status='failed',error_message=?,updated_at=? WHERE album_id=? AND idempotency_key=?`, err.Error(), time.Now().Unix(), albumID, idempotencyKey)
		return Media{}, err
	}
	obj.RelativePath = target
	_, mediaID, err := s.upsertObject(ctx, albumID, target, kind, mime, obj)
	if err != nil {
		return Media{}, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE upload_requests SET status='succeeded',media_id=?,temp_path='',updated_at=? WHERE album_id=? AND idempotency_key=?`, mediaID, time.Now().Unix(), albumID, idempotencyKey)
	_, _ = s.queue.Enqueue(ctx, "thumbnail", "thumb:"+mediaID+":"+fingerprint(obj), map[string]any{"albumId": albumID, "mediaId": mediaID}, time.Now())
	if album.Backup != nil && album.BackupEnabled && album.SyncOnUpload {
		_, _ = s.queue.Enqueue(ctx, "backup", "backup:"+mediaID+":"+fingerprint(obj), map[string]any{"albumId": albumID, "mediaId": mediaID}, time.Now())
	}
	return s.GetMedia(ctx, mediaID)
}
func (s *Service) BackupJob(ctx context.Context, job jobs.Job) error {
	var payload struct {
		AlbumID string `json:"albumId"`
		MediaID string `json:"mediaId"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	return s.Backup(ctx, payload.AlbumID, payload.MediaID)
}
func (s *Service) Backup(ctx context.Context, albumID, mediaID string) error {
	album, err := s.GetAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	if album.Backup == nil || !album.BackupEnabled {
		return nil
	}
	m, err := s.GetMedia(ctx, mediaID)
	if err != nil {
		return err
	}
	if m.Missing {
		return nil
	}
	var existingVersion, status string
	err = s.db.QueryRowContext(ctx, `SELECT source_version,status FROM media_replicas WHERE media_id=? AND storage_id=?`, mediaID, album.Backup.StorageID).Scan(&existingVersion, &status)
	if err == nil && existingVersion == m.SourceVersion && status == "ready" {
		return nil
	}
	primary, err := s.storages.Provider(ctx, album.Primary.StorageID)
	if err != nil {
		return err
	}
	backup, err := s.storages.Provider(ctx, album.Backup.StorageID)
	if err != nil {
		return err
	}
	reader, obj, err := primary.Open(ctx, joinPath(album.Primary.RootPath, m.RelativePath), nil)
	if errors.Is(err, storage.ErrUnauthorized) {
		return jobs.ErrWaitingAuth
	}
	if err != nil {
		return err
	}
	defer reader.Close()
	tmp, err := os.CreateTemp(s.tempDir, "backup-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	written, err := copyContext(ctx, tmp, reader)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if written != obj.Size {
		return fmt.Errorf("source size changed during backup")
	}
	if fingerprint(obj) != m.SourceVersion {
		return fmt.Errorf("source version changed during backup")
	}
	target := joinPath(album.Backup.RootPath, m.RelativePath)
	stored, err := backup.Put(ctx, target, storage.FileSource{Path: tmpPath, ByteSize: written}, storage.PutOptions{Conflict: "overwrite"})
	if errors.Is(err, storage.ErrUnauthorized) {
		return jobs.ErrWaitingAuth
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO media_replicas(id,media_id,storage_id,relative_path,provider_object_id,source_version,status,verified_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(media_id,storage_id) DO UPDATE SET relative_path=excluded.relative_path,provider_object_id=excluded.provider_object_id,source_version=excluded.source_version,status='ready',last_error='',verified_at=excluded.verified_at,delete_after=NULL,delete_suppressed=0,updated_at=excluded.updated_at`, newID(), mediaID, album.Backup.StorageID, m.RelativePath, stored.ID, m.SourceVersion, "ready", now.Unix(), now.Unix(), now.Unix())
	return err
}
func (s *Service) MirrorDeleteJob(ctx context.Context, job jobs.Job) error {
	var payload struct {
		AlbumID string `json:"albumId"`
		MediaID string `json:"mediaId"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	return s.executeMirrorDelete(ctx, payload.AlbumID, payload.MediaID)
}
func (s *Service) CancelMirrorDelete(ctx context.Context, mediaID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media_items SET delete_suppressed=1 WHERE id=?`, mediaID)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE media_replicas SET delete_suppressed=1,delete_after=NULL,status=CASE WHEN status='pending-delete' THEN 'ready' ELSE status END WHERE media_id=?`, mediaID)
	}
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE jobs SET state='cancelled',lease_until=NULL,updated_at=? WHERE dedupe_key=? AND state IN ('pending','waiting-auth')`, time.Now().Unix(), "mirror-delete:"+mediaID)
	}
	return err
}
func (s *Service) ResumeMirrorDelete(ctx context.Context, mediaID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media_items SET delete_suppressed=0 WHERE id=?`, mediaID)
	if err != nil {
		return err
	}
	m, err := s.GetMedia(ctx, mediaID)
	if err != nil {
		return err
	}
	album, err := s.GetAlbum(ctx, m.AlbumID)
	if err != nil {
		return err
	}
	s.scheduleMirrorDelete(ctx, album, mediaID, time.Now().Add(-s.mirrorGrace))
	return nil
}
func (s *Service) scheduleMirrorDelete(ctx context.Context, album Album, mediaID string, first time.Time) {
	var suppressed int
	_ = s.db.QueryRowContext(ctx, `SELECT delete_suppressed FROM media_items WHERE id=?`, mediaID).Scan(&suppressed)
	if suppressed == 1 {
		return
	}
	executeAt := first.Add(s.mirrorGrace)
	if executeAt.Before(time.Now()) {
		executeAt = time.Now()
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE media_replicas SET status='pending-delete',delete_after=?,updated_at=? WHERE media_id=? AND storage_id=?`, executeAt.Unix(), time.Now().Unix(), mediaID, album.Backup.StorageID)
	_, _ = s.queue.Enqueue(ctx, "mirror-delete", "mirror-delete:"+mediaID, map[string]any{"albumId": album.ID, "mediaId": mediaID}, executeAt)
}
func (s *Service) executeMirrorDelete(ctx context.Context, albumID, mediaID string) error {
	album, err := s.GetAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	if album.BackupMode != "mirror" || album.Backup == nil {
		return nil
	}
	m, err := s.GetMedia(ctx, mediaID)
	if err != nil {
		return err
	}
	var missing, suppressed int
	var first sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT missing_scans,delete_suppressed,first_missing_at FROM media_items WHERE id=?`, mediaID).Scan(&missing, &suppressed, &first); err != nil {
		return err
	}
	if missing < 2 || suppressed == 1 || !first.Valid || time.Now().Before(time.Unix(first.Int64, 0).Add(s.mirrorGrace)) {
		return nil
	}
	primary, err := s.storages.Provider(ctx, album.Primary.StorageID)
	if err != nil {
		return err
	}
	if err := primary.Validate(ctx); err != nil {
		return err
	}
	if _, err := primary.Stat(ctx, joinPath(album.Primary.RootPath, m.RelativePath)); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	backup, err := s.storages.Provider(ctx, album.Backup.StorageID)
	if err != nil {
		return err
	}
	if err := backup.Delete(ctx, joinPath(album.Backup.RootPath, m.RelativePath)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE media_replicas SET status='deleted',delete_after=NULL,updated_at=? WHERE media_id=? AND storage_id=?`, time.Now().Unix(), mediaID, album.Backup.StorageID)
	return err
}
func (s *Service) upsertObject(ctx context.Context, albumID, rel, kind, mime string, obj storage.Object) (bool, string, error) {
	normalized := normalizePath(rel)
	version := fingerprint(obj)
	var id, oldVersion string
	err := s.db.QueryRowContext(ctx, `SELECT id,source_version FROM media_items WHERE album_id=? AND normalized_path=?`, albumID, normalized).Scan(&id, &oldVersion)
	now := time.Now().UTC()
	if err == sql.ErrNoRows {
		id = newID()
		_, err = s.db.ExecContext(ctx, `INSERT INTO media_items(id,album_id,relative_path,normalized_path,provider_object_id,kind,mime_type,size,modified_at,etag,native_checksum,source_version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, albumID, rel, normalized, obj.ID, kind, mime, obj.Size, obj.ModifiedAt.Unix(), obj.ETag, obj.NativeChecksum, version, now.Unix(), now.Unix())
		return true, id, err
	}
	if err != nil {
		return false, "", err
	}
	changed := oldVersion != version
	_, err = s.db.ExecContext(ctx, `UPDATE media_items SET relative_path=?,provider_object_id=?,kind=?,mime_type=?,size=?,modified_at=?,etag=?,native_checksum=?,source_version=?,processing_status=CASE WHEN source_version<>? THEN 'pending' ELSE processing_status END,missing_scans=0,first_missing_at=NULL,updated_at=? WHERE id=?`, rel, obj.ID, kind, mime, obj.Size, obj.ModifiedAt.Unix(), obj.ETag, obj.NativeChecksum, version, version, now.Unix(), id)
	return changed, id, err
}
func (s *Service) ensureNoOverlap(ctx context.Context, storageID, root, excludeAlbum string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT b.album_id,b.root_path FROM album_storage_bindings b JOIN albums a ON a.id=b.album_id WHERE b.storage_id=? AND a.removed_at IS NULL AND b.album_id<>?`, storageID, excludeAlbum)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var albumID, existing string
		_ = rows.Scan(&albumID, &existing)
		if pathsOverlap(root, existing) {
			return fmt.Errorf("storage path overlaps album %s", albumID)
		}
	}
	return rows.Err()
}
func (s *Service) setTagsTx(ctx context.Context, tx *sql.Tx, albumID string, tags []string) error {
	if len(tags) > 20 {
		return errors.New("an album can have at most 20 tags")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM album_tags WHERE album_id=?`, albumID); err != nil {
		return err
	}
	for _, tag := range tags {
		name := strings.TrimSpace(norm.NFC.String(tag))
		if name == "" {
			continue
		}
		if len(name) > 40 {
			return errors.New("tag must not exceed 40 characters")
		}
		normalized := normalizeTag(name)
		id := newID()
		_, err := tx.ExecContext(ctx, `INSERT INTO tags(id,name,normalized_name,created_at) VALUES(?,?,?,?) ON CONFLICT(normalized_name) DO UPDATE SET name=tags.name`, id, name, normalized, time.Now().Unix())
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE normalized_name=?`, normalized).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO album_tags(album_id,tag_id) VALUES(?,?)`, albumID, id); err != nil {
			return err
		}
	}
	return nil
}
func prepareBackupRoot(ctx context.Context, provider storage.Provider, root, albumID string) error {
	if err := provider.Mkdir(ctx, root); err != nil {
		return fmt.Errorf("prepare backup directory: %w", err)
	}
	page, err := provider.List(ctx, root, "", 3)
	if err != nil {
		return err
	}
	markerPath := joinPath(root, ".ialbum-album.json")
	if len(page.Objects) > 0 {
		for _, obj := range page.Objects {
			if obj.RelativePath == markerPath || path.Base(obj.RelativePath) == ".ialbum-album.json" {
				reader, _, err := provider.Open(ctx, markerPath, nil)
				if err != nil {
					return err
				}
				raw, _ := io.ReadAll(io.LimitReader(reader, 4096))
				reader.Close()
				var marker struct {
					AlbumID string `json:"albumId"`
				}
				if json.Unmarshal(raw, &marker) == nil && marker.AlbumID == albumID {
					return nil
				}
			}
		}
		return errors.New("backup directory must be empty or owned by this album")
	}
	raw, _ := json.Marshal(map[string]any{"albumId": albumID, "version": 1})
	_, err = provider.Put(ctx, markerPath, storage.BytesSource(raw), storage.PutOptions{Conflict: "fail"})
	return err
}
func walkProvider(ctx context.Context, provider storage.Provider, root string) ([]storage.Object, error) {
	queue := []string{root}
	var result []storage.Object
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		cursor := ""
		for {
			page, err := provider.List(ctx, dir, cursor, 200)
			if err != nil {
				return nil, err
			}
			for _, obj := range page.Objects {
				if obj.IsDir {
					queue = append(queue, obj.RelativePath)
				} else {
					result = append(result, obj)
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return result, nil
}
func availableName(ctx context.Context, provider storage.Provider, root, filename string) (string, error) {
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	ext := path.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 0; i < 10000; i++ {
		candidate := filename
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		_, err := provider.Stat(ctx, joinPath(root, candidate))
		if errors.Is(err, storage.ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not find an available filename")
}
func classify(name string) (string, string, bool) {
	ext := strings.ToLower(path.Ext(name))
	if mime, ok := supportedImages[ext]; ok {
		return "image", mime, true
	}
	if mime, ok := supportedVideos[ext]; ok {
		return "video", mime, true
	}
	return "", "", false
}
func fingerprint(obj storage.Object) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s", obj.ID, obj.Size, obj.ModifiedAt.UnixNano(), obj.ETag, obj.NativeChecksum)))
	return hex.EncodeToString(sum[:])
}
func normalizePath(value string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimPrefix(path.Clean("/"+value), "/")))
}
func normalizeTag(value string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(value)))
}
func pathsOverlap(a, b string) bool {
	a = strings.Trim(strings.ToLower(path.Clean("/"+a)), "/")
	b = strings.Trim(strings.ToLower(path.Clean("/"+b)), "/")
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func joinPath(parts ...string) string {
	var nonempty []string
	for _, part := range parts {
		if part = strings.Trim(part, "/"); part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return path.Join(nonempty...)
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, er := src.Read(buf)
		if n > 0 {
			wn, ew := dst.Write(buf[:n])
			total += int64(wn)
			if ew != nil {
				return total, ew
			}
			if wn != n {
				return total, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return total, nil
		}
		if er != nil {
			return total, er
		}
	}
}
func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

var ErrUnsupportedMedia = errors.New("unsupported media type")
