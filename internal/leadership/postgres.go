package leadership

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Name                string
	DSN                 string
	Key                 int64
	RetryInterval       time.Duration
	HealthcheckInterval time.Duration
}

func Run(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	if strings.TrimSpace(cfg.DSN) == "" {
		return fn(ctx)
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("leader name is required")
	}
	if cfg.Key == 0 {
		cfg.Key = deriveKey(cfg.Name)
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 2 * time.Second
	}
	if cfg.HealthcheckInterval <= 0 {
		cfg.HealthcheckInterval = 5 * time.Second
	}

	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, err := pool.Acquire(ctx)
		if err != nil {
			return err
		}
		acquired, err := tryAcquire(ctx, conn, cfg.Key)
		if err != nil {
			conn.Release()
			return err
		}
		if !acquired {
			conn.Release()
			if err := sleepContext(ctx, cfg.RetryInterval); err != nil {
				return err
			}
			continue
		}

		slog.Info("leader lock acquired", "name", cfg.Name)
		runCtx, cancel := context.WithCancel(ctx)
		taskErrCh := make(chan error, 1)
		go func() {
			taskErrCh <- fn(runCtx)
		}()

		heartbeat := time.NewTicker(cfg.HealthcheckInterval)
		lostLeadership := false
		var taskErr error

	loop:
		for {
			select {
			case <-ctx.Done():
				taskErr = ctx.Err()
				cancel()
				break loop
			case taskErr = <-taskErrCh:
				break loop
			case <-heartbeat.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, cfg.HealthcheckInterval)
				err := pingLockConnection(pingCtx, conn)
				pingCancel()
				if err != nil {
					lostLeadership = true
					cancel()
					break loop
				}
			}
		}

		heartbeat.Stop()
		if lostLeadership {
			slog.Warn("leader lock lost; background loops will be restarted after reacquire", "name", cfg.Name)
			waitForTaskExit(taskErrCh)
			_ = unlock(context.Background(), conn, cfg.Key)
			conn.Release()
			if err := sleepContext(ctx, cfg.RetryInterval); err != nil {
				return err
			}
			continue
		}

		cancel()
		if err := unlock(context.Background(), conn, cfg.Key); err != nil {
			slog.Warn("leader lock release failed", "name", cfg.Name, "err", err)
		}
		conn.Release()
		if errors.Is(taskErr, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		return taskErr
	}
}

func tryAcquire(ctx context.Context, conn *pgxpool.Conn, key int64) (bool, error) {
	var acquired bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		return false, err
	}
	return acquired, nil
}

func pingLockConnection(ctx context.Context, conn *pgxpool.Conn) error {
	var ok int
	return conn.QueryRow(ctx, "select 1").Scan(&ok)
}

func unlock(ctx context.Context, conn *pgxpool.Conn, key int64) error {
	unlockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRow(unlockCtx, "select pg_advisory_unlock($1)", key).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked {
		return fmt.Errorf("advisory lock %d was not held", key)
	}
	return nil
}

func waitForTaskExit(taskErrCh <-chan error) {
	select {
	case <-taskErrCh:
	case <-time.After(5 * time.Second):
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func deriveKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
