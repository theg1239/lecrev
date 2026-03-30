package postgres

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func (s *Store) RunMigrations(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `
		create table if not exists schema_migrations (
			version text primary key,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	for _, entry := range entries {
		var applied bool
		if err := s.pool.QueryRow(ctx, `select exists(select 1 from schema_migrations where version = $1)`, entry.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err := conn.Conn().PgConn().Exec(ctx, string(sqlBytes)).ReadAll(); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := s.pool.Exec(ctx, `insert into schema_migrations (version) values ($1)`, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

var _ pgx.Tx
