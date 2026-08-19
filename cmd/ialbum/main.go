package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aidc-lab/ialbum/internal/api"
	"github.com/aidc-lab/ialbum/internal/auth"
	"github.com/aidc-lab/ialbum/internal/config"
	appdb "github.com/aidc-lab/ialbum/internal/db"
	"github.com/aidc-lab/ialbum/internal/jobs"
	"github.com/aidc-lab/ialbum/internal/media"
	storagemanager "github.com/aidc-lab/ialbum/internal/storage/manager"
	webassets "github.com/aidc-lab/ialbum/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) > 2 && os.Args[1] == "admin" && os.Args[2] == "reset-password" {
		if err := resetPassword(os.Args[3:]); err != nil {
			logger.Error("password reset failed", "error", err)
			os.Exit(1)
		}
		logger.Info("password reset complete")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	database, err := appdb.Open(cfg.DatabasePath)
	if err != nil {
		logger.Error("database failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	sealer, err := auth.NewSealer(cfg.MasterKey)
	if err != nil {
		logger.Error("secret encryption failed", "error", err)
		os.Exit(1)
	}
	authService, err := auth.NewService(database, cfg.SetupTokenTTL, cfg.SessionIdleTTL, cfg.SessionAbsoluteTTL)
	if err != nil {
		logger.Error("authentication failed", "error", err)
		os.Exit(1)
	}
	if token, expires, ok := authService.SetupToken(); ok {
		logger.Warn("ialbum setup required", "setupToken", token, "expiresAt", expires.Format(time.RFC3339))
	}
	storageManager := storagemanager.NewManager(database, sealer)
	if err := storageManager.ValidateSecrets(context.Background()); err != nil {
		logger.Error("stored credentials cannot be decrypted", "error", err)
		os.Exit(1)
	}
	queue := jobs.New(database, cfg.GlobalWorkerCount)
	catalog := media.NewService(database, storageManager, queue, cfg.TempDir, cfg.CacheDir, cfg.MirrorDeletionGrace)
	processor := media.NewProcessor(database, catalog, cfg.CacheDir, cfg.TempDir, cfg.FFmpegPath, cfg.FFprobePath, cfg.MaxVideoStagingBytes)
	queue.Register("scan", catalog.ScanJob)
	queue.Register("backup", catalog.BackupJob)
	queue.Register("mirror-delete", catalog.MirrorDeleteJob)
	queue.Register("thumbnail", processor.Job)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	queue.Start(ctx)
	defer queue.Stop()
	go schedule(ctx, catalog, processor, cfg.CacheMaxBytes, logger)
	embedded, root := webassets.Assets()
	assetFS, err := fs.Sub(embedded, root)
	if err != nil {
		logger.Error("frontend assets failed", "error", err)
		os.Exit(1)
	}
	handler := api.New(cfg, database, authService, storageManager, catalog, processor, queue, logger, assetFS).Handler()
	server := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	go func() {
		logger.Info("ialbum listening", "address", cfg.Listen, "dataDir", cfg.DataDir, "ffmpeg", processor.Available())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func schedule(ctx context.Context, catalog *media.Service, processor *media.Processor, cacheMax int64, logger *slog.Logger) {
	scanTicker := time.NewTicker(time.Minute)
	cacheTicker := time.NewTicker(time.Hour)
	defer scanTicker.Stop()
	defer cacheTicker.Stop()
	_ = catalog.ScheduleDueScans(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-scanTicker.C:
			if err := catalog.ScheduleDueScans(ctx); err != nil {
				logger.Warn("scan scheduler failed", "error", err)
			}
		case <-cacheTicker.C:
			if err := processor.Evict(ctx, cacheMax); err != nil {
				logger.Warn("cache eviction failed", "error", err)
			}
		}
	}
}
func resetPassword(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	username := flags.String("username", "admin", "administrator username")
	password := flags.String("password", "", "new password (or set IALBUM_NEW_PASSWORD)")
	dataDir := flags.String("data-dir", cfg.DataDir, "ialbum data directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *password == "" {
		*password = os.Getenv("IALBUM_NEW_PASSWORD")
	}
	if *password == "" {
		return errors.New("provide --password or IALBUM_NEW_PASSWORD")
	}
	return auth.ResetPassword(context.Background(), *dataDir+string(os.PathSeparator)+"ialbum.db", *username, *password)
}
