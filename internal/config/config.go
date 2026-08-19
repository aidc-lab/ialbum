package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListen       = "127.0.0.1:8080"
	defaultMaxUpload    = int64(20 << 30)
	defaultCacheBytes   = int64(5 << 30)
	defaultVideoStaging = int64(2 << 30)
)

type Config struct {
	Listen               string
	DataDir              string
	DatabasePath         string
	CacheDir             string
	TempDir              string
	MasterKeyPath        string
	MasterKey            []byte
	PublicURL            string
	AllowInsecureLAN     bool
	TrustedProxies       []string
	MaxUploadBytes       int64
	CacheMaxBytes        int64
	MaxVideoStagingBytes int64
	FFmpegPath           string
	FFprobePath          string
	SetupTokenTTL        time.Duration
	SessionIdleTTL       time.Duration
	SessionAbsoluteTTL   time.Duration
	DefaultScanInterval  time.Duration
	MirrorDeletionGrace  time.Duration
	GlobalWorkerCount    int
	PerConnectionWorkers int
	DownloadConcurrency  int
}

func Load() (Config, error) {
	dataDir := strings.TrimSpace(os.Getenv("IALBUM_DATA_DIR"))
	if dataDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve user config dir: %w", err)
		}
		dataDir = filepath.Join(base, "ialbum")
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("secure data dir: %w", err)
	}
	cacheDir := filepath.Join(dataDir, "cache")
	tempDir := filepath.Join(dataDir, "tmp")
	for _, dir := range []string{cacheDir, tempDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Config{}, fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return Config{}, fmt.Errorf("secure %s: %w", dir, err)
		}
	}

	listen := envOr("IALBUM_LISTEN", defaultListen)
	allowInsecure := envBool("IALBUM_ALLOW_INSECURE_LAN", false)
	if err := validateListen(listen, envOr("IALBUM_PUBLIC_URL", ""), allowInsecure); err != nil {
		return Config{}, err
	}

	masterKeyPath := filepath.Join(dataDir, "master.key")
	masterKey, err := loadMasterKey(masterKeyPath)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen: listen, DataDir: dataDir,
		DatabasePath: filepath.Join(dataDir, "ialbum.db"),
		CacheDir:     cacheDir, TempDir: tempDir, MasterKeyPath: masterKeyPath, MasterKey: masterKey,
		PublicURL: envOr("IALBUM_PUBLIC_URL", ""), AllowInsecureLAN: allowInsecure,
		TrustedProxies:       splitCSV(os.Getenv("IALBUM_TRUSTED_PROXIES")),
		MaxUploadBytes:       envInt64("IALBUM_MAX_UPLOAD_BYTES", defaultMaxUpload),
		CacheMaxBytes:        envInt64("IALBUM_CACHE_MAX_BYTES", defaultCacheBytes),
		MaxVideoStagingBytes: envInt64("IALBUM_MAX_VIDEO_STAGING_BYTES", defaultVideoStaging),
		FFmpegPath:           os.Getenv("IALBUM_FFMPEG"), FFprobePath: os.Getenv("IALBUM_FFPROBE"),
		SetupTokenTTL: 15 * time.Minute, SessionIdleTTL: 12 * time.Hour,
		SessionAbsoluteTTL: 7 * 24 * time.Hour, DefaultScanInterval: 6 * time.Hour,
		MirrorDeletionGrace: 24 * time.Hour, GlobalWorkerCount: 2,
		PerConnectionWorkers: 1, DownloadConcurrency: 2,
	}, nil
}

func validateListen(listen, publicURL string, allowInsecure bool) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid IALBUM_LISTEN: %w", err)
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if loopback {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(publicURL), "https://") || allowInsecure {
		return nil
	}
	return errors.New("non-loopback listen requires HTTPS IALBUM_PUBLIC_URL or IALBUM_ALLOW_INSECURE_LAN=true")
}

func loadMasterKey(path string) ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv("IALBUM_MASTER_KEY")); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) != 32 {
			return nil, errors.New("IALBUM_MASTER_KEY must be a base64 encoded 32-byte key")
		}
		return key, nil
	}
	if raw, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, errors.New("invalid master.key; refusing to replace it")
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("save master key: %w", err)
	}
	return key, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err == nil {
			return value
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && value > 0 {
			return value
		}
	}
	return fallback
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
