// Command server runs the badminton RSVP app: one process, one SQLite file,
// one container.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "time/tzdata" // embed the timezone database so the image needs no tzdata package

	"badminton/internal/config"
	"badminton/internal/mail"
	"badminton/internal/store"
	"badminton/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if _, err := st.EnsureOrganizer(context.Background(), cfg.OrganizerName, cfg.OrganizerEmail, cfg.OrganizerPassword); err != nil {
		return err
	}

	// A missing or broken SMTP config must not stop the app from booting: the
	// organizer still needs the dashboard, and the delivery log will say why
	// nothing went out.
	var sender mail.Sender
	if smtp, err := mail.NewSMTPSender(cfg.SMTP); err != nil {
		log.Warn("email delivery is disabled", "reason", err)
		sender = mail.DisabledSender{Reason: err}
	} else {
		sender = smtp
		log.Info("smtp configured", "host", cfg.SMTP.Host, "port", cfg.SMTP.Port, "tls", cfg.SMTP.TLS)
	}

	worker := mail.NewWorker(st, sender, log, cfg.SMTP.RatePerS)
	srv, err := web.New(cfg, st, worker, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go worker.Run(ctx)
	if cfg.BackupDir != "" {
		go runBackups(ctx, st, cfg, log)
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "tz", cfg.Timezone.String())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func logLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// runBackups takes a nightly snapshot with SQLite's VACUUM INTO and prunes old
// ones, so a restorable copy exists without any external tooling on the VM.
func runBackups(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	backupOnce(ctx, st, cfg, log)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			backupOnce(ctx, st, cfg, log)
		}
	}
}

func backupOnce(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) {
	name := "badminton-" + time.Now().In(cfg.Timezone).Format("2006-01-02") + ".db"
	path := filepath.Join(cfg.BackupDir, name)
	if err := st.Backup(ctx, path); err != nil {
		log.Error("backup failed", "path", path, "err", err)
		return
	}
	log.Info("backup written", "path", path)

	entries, err := os.ReadDir(cfg.BackupDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-cfg.BackupKeepFor)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if filepath.Ext(e.Name()) == ".db" {
			os.Remove(filepath.Join(cfg.BackupDir, e.Name()))
		}
	}
}
