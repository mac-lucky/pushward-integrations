package state

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a pgxpool.Pool with optional password file support.
// When passwordFile is non-empty, BeforeConnect reads the password on each
// new connection and a background goroutine watches for file changes to
// reset the pool (supporting live credential rotation).
func NewPool(ctx context.Context, dsn, passwordFile string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing database DSN: %w", err)
	}

	if passwordFile != "" {
		pw, err := readPasswordFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("reading initial password file: %w", err)
		}
		poolCfg.ConnConfig.Password = pw

		poolCfg.BeforeConnect = func(_ context.Context, cfg *pgx.ConnConfig) error {
			pw, err := readPasswordFile(passwordFile)
			if err != nil {
				slog.Error("failed to read password file for connection", "error", err)
				return err
			}
			cfg.Password = pw
			return nil
		}
	}

	pool, err := connectWithRetry(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	if passwordFile != "" {
		go watchPasswordFile(ctx, pool, passwordFile)
	}

	return pool, nil
}

func connectWithRetry(ctx context.Context, poolCfg *pgxpool.Config) (*pgxpool.Pool, error) {
	const maxRetries = 10
	delay := time.Second
	var pool *pgxpool.Pool
	var err error

	backoff := func(msg string, attempt int, err error) error {
		if attempt == maxRetries {
			slog.Error(msg+", giving up", "attempt", attempt, "error", err)
			return nil
		}
		slog.Warn(msg+", retrying...", "attempt", attempt, "max", maxRetries, "error", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("connection cancelled: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
		return nil
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			if waitErr := backoff("failed to create database pool", attempt, err); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if err = pool.Ping(ctx); err != nil {
			pool.Close()
			if waitErr := backoff("failed to ping database", attempt, err); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		break
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to database after %d attempts: %w", maxRetries, err)
	}
	return pool, nil
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func watchPasswordFile(ctx context.Context, pool *pgxpool.Pool, passwordFile string) {
	dir := filepath.Dir(passwordFile)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("failed to create password file watcher", "error", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		slog.Error("failed to watch password directory", "dir", dir, "error", err)
		return
	}
	slog.Info("watching password file for rotation", "dir", dir)
	baseName := filepath.Base(passwordFile)

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				if event.Name != passwordFile && filepath.Base(event.Name) != baseName {
					continue
				}
				slog.Info("password file changed, resetting connection pool", "event", event.Name)
				pool.Reset()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("password file watcher error", "error", err)
		}
	}
}
