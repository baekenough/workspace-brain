package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies all SQL files found in the embedded migrations directory in
// lexicographic order. Each file is executed as a single transaction so that a
// partial failure leaves the schema unmodified.
func migrate(ctx context.Context, conn *pgx.Conn) error {
	// Collect migration file names.
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("postgres migrate: read migrations dir: %w", err)
	}

	// Sort ensures deterministic application order (001_, 002_, …).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		path := "migrations/" + entry.Name()
		data, err := migrationsFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("postgres migrate: read %s: %w", path, err)
		}
		if err := applyMigration(ctx, conn, entry.Name(), string(data)); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, name, sql string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres migrate %s: begin: %w", name, err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("postgres migrate %s: exec: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres migrate %s: commit: %w", name, err)
	}
	return nil
}
