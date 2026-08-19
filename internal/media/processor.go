package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"

	appdb "github.com/aidc-lab/ialbum/internal/db"
	"github.com/aidc-lab/ialbum/internal/jobs"
)

var ErrThumbnailPending = errors.New("thumbnail is not ready")

type Processor struct {
	db                                 *appdb.DB
	catalog                            *Service
	cacheDir, tempDir, ffmpeg, ffprobe string
	maxVideoBytes                      int64
}

func NewProcessor(db *appdb.DB, catalog *Service, cacheDir, tempDir, ffmpeg, ffprobe string, maxVideoBytes int64) *Processor {
	if ffmpeg == "" {
		ffmpeg, _ = exec.LookPath("ffmpeg")
	}
	if ffprobe == "" {
		ffprobe, _ = exec.LookPath("ffprobe")
	}
	return &Processor{db: db, catalog: catalog, cacheDir: cacheDir, tempDir: tempDir, ffmpeg: ffmpeg, ffprobe: ffprobe, maxVideoBytes: maxVideoBytes}
}
func (p *Processor) Available() bool { return p.ffmpeg != "" && p.ffprobe != "" }
func (p *Processor) Job(ctx context.Context, job jobs.Job) error {
	var payload struct {
		MediaID string `json:"mediaId"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	return p.Process(ctx, payload.MediaID)
}
func (p *Processor) Process(ctx context.Context, mediaID string) error {
	item, err := p.catalog.GetMedia(ctx, mediaID)
	if err != nil {
		return err
	}
	if item.Missing {
		return nil
	}
	reader, _, _, err := p.catalog.OpenMedia(ctx, mediaID, nil)
	if err != nil {
		return err
	}
	defer reader.Close()
	tmp, err := os.CreateTemp(p.tempDir, "media-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = copyContext(ctx, tmp, reader); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if item.Kind == "video" {
		if !p.Available() || item.Size > p.maxVideoBytes {
			_, err = p.db.ExecContext(ctx, `UPDATE media_items SET processing_status='unavailable',updated_at=? WHERE id=?`, time.Now().Unix(), mediaID)
			return err
		}
		return p.processVideo(ctx, item, tmpPath)
	}
	return p.processImage(ctx, item, tmpPath)
}
func (p *Processor) processImage(ctx context.Context, item Media, source string) error {
	file, err := os.Open(source)
	if err != nil {
		return p.markFailed(ctx, item.ID, err)
	}
	config, _, configErr := image.DecodeConfig(file)
	_ = file.Close()
	if configErr != nil {
		return p.markFailed(ctx, item.ID, configErr)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 100_000_000 {
		return p.markFailed(ctx, item.ID, errors.New("image dimensions exceed the safe processing limit"))
	}
	img, err := imaging.Open(source, imaging.AutoOrientation(true))
	if err != nil {
		_, _ = p.db.ExecContext(ctx, `UPDATE media_items SET processing_status='failed',updated_at=? WHERE id=?`, time.Now().Unix(), item.ID)
		return err
	}
	bounds := img.Bounds()
	variants := map[string]int{"grid": 320, "preview": 1280}
	for name, width := range variants {
		resized := img
		if bounds.Dx() > width {
			resized = imaging.Resize(img, width, 0, imaging.Lanczos)
		}
		if err := p.saveVariant(ctx, item, name, resized); err != nil {
			return err
		}
	}
	_, err = p.db.ExecContext(ctx, `UPDATE media_items SET width=?,height=?,processing_status='ready',updated_at=? WHERE id=?`, bounds.Dx(), bounds.Dy(), time.Now().Unix(), item.ID)
	return err
}
func (p *Processor) processVideo(parent context.Context, item Media, source string) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	probe := exec.CommandContext(ctx, p.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height:format=duration", "-of", "json", source)
	raw, err := probe.Output()
	if err != nil {
		return p.markFailed(ctx, item.ID, err)
	}
	var result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return p.markFailed(ctx, item.ID, err)
	}
	poster, err := os.CreateTemp(p.tempDir, "poster-*.jpg")
	if err != nil {
		return err
	}
	posterPath := poster.Name()
	poster.Close()
	defer os.Remove(posterPath)
	command := exec.CommandContext(ctx, p.ffmpeg, "-hide_banner", "-loglevel", "error", "-ss", "1", "-i", source, "-frames:v", "1", "-vf", "scale=min(1280\\,iw):-2", "-y", posterPath)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		return p.markFailed(ctx, item.ID, fmt.Errorf("ffmpeg: %w: %s", runErr, strings.TrimSpace(string(output))))
	}
	img, err := imaging.Open(posterPath)
	if err != nil {
		return p.markFailed(ctx, item.ID, err)
	}
	for name, width := range map[string]int{"grid": 320, "preview": 1280} {
		resized := img
		if img.Bounds().Dx() > width {
			resized = imaging.Resize(img, width, 0, imaging.Lanczos)
		}
		if err := p.saveVariant(ctx, item, name, resized); err != nil {
			return err
		}
	}
	var width, height int
	if len(result.Streams) > 0 {
		width = result.Streams[0].Width
		height = result.Streams[0].Height
	}
	durationFloat, _ := strconv.ParseFloat(result.Format.Duration, 64)
	_, err = p.db.ExecContext(ctx, `UPDATE media_items SET width=?,height=?,duration_ms=?,processing_status='ready',updated_at=? WHERE id=?`, width, height, int64(durationFloat*1000), time.Now().Unix(), item.ID)
	return err
}
func (p *Processor) saveVariant(ctx context.Context, item Media, variant string, img image.Image) error {
	dir := filepath.Join(p.cacheDir, item.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	filename := item.SourceVersion + "-" + variant + ".jpg"
	target := filepath.Join(dir, filename)
	tmp := target + ".tmp.jpg"
	if err := imaging.Save(img, tmp, imaging.JPEGQuality(85)); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = p.db.ExecContext(ctx, `INSERT INTO thumbnail_variants(id,media_id,source_version,variant,cache_path,byte_size,status,last_accessed_at,created_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(media_id,source_version,variant) DO UPDATE SET cache_path=excluded.cache_path,byte_size=excluded.byte_size,status='ready',error_message='',last_accessed_at=excluded.last_accessed_at`, newID(), item.ID, item.SourceVersion, variant, target, info.Size(), "ready", now.Unix(), now.Unix())
	return err
}
func (p *Processor) OpenThumbnail(ctx context.Context, mediaID, variant string) (*os.File, string, error) {
	if variant != "grid" && variant != "preview" {
		return nil, "", errors.New("invalid thumbnail variant")
	}
	var cachePath, sourceVersion, status string
	err := p.db.QueryRowContext(ctx, `SELECT tv.cache_path,tv.source_version,tv.status FROM thumbnail_variants tv JOIN media_items m ON m.id=tv.media_id WHERE tv.media_id=? AND tv.variant=? AND tv.source_version=m.source_version`, mediaID, variant).Scan(&cachePath, &sourceVersion, &status)
	if err != nil || status != "ready" {
		return nil, "", ErrThumbnailPending
	}
	file, err := os.Open(cachePath)
	if err != nil {
		return nil, "", ErrThumbnailPending
	}
	_, _ = p.db.ExecContext(ctx, `UPDATE thumbnail_variants SET last_accessed_at=? WHERE media_id=? AND source_version=? AND variant=?`, time.Now().Unix(), mediaID, sourceVersion, variant)
	return file, sourceVersion, nil
}
func (p *Processor) markFailed(ctx context.Context, mediaID string, err error) error {
	_, _ = p.db.ExecContext(context.Background(), `UPDATE media_items SET processing_status='failed',updated_at=? WHERE id=?`, time.Now().Unix(), mediaID)
	return err
}
func (p *Processor) Evict(ctx context.Context, maxBytes int64) error {
	var total int64
	if err := p.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(byte_size),0) FROM thumbnail_variants WHERE status='ready'`).Scan(&total); err != nil {
		return err
	}
	for total > maxBytes {
		var id, cachePath string
		var size int64
		err := p.db.QueryRowContext(ctx, `SELECT id,cache_path,byte_size FROM thumbnail_variants WHERE status='ready' ORDER BY last_accessed_at LIMIT 1`).Scan(&id, &cachePath, &size)
		if err != nil {
			return err
		}
		_ = os.Remove(cachePath)
		_, _ = p.db.ExecContext(ctx, `DELETE FROM thumbnail_variants WHERE id=?`, id)
		total -= size
	}
	return nil
}
