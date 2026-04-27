package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

func tableHasColumn(db *sql.DB, table, column string) bool {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return false
	}
	return n > 0
}

// Migration represents a database schema change
type Migration struct {
	ID          int
	Name        string
	AppliedAt   time.Time
	MigrateFunc func(*sql.DB) error
}

// initMigrations creates the migrations table if it doesn't exist
func initMigrations(db *sql.DB) error {
	query := `
	CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := db.Exec(query)
	return err
}

// isMigrationApplied checks if a migration has already been run
func isMigrationApplied(db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE name = ?`, name).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// markMigrationApplied records that a migration has been completed
func markMigrationApplied(db *sql.DB, name string) error {
	_, err := db.Exec(`INSERT INTO migrations (name) VALUES (?)`, name)
	return err
}

// runMigrations executes all pending migrations
func runMigrations(db *sql.DB) error {
	// Ensure migrations table exists
	if err := initMigrations(db); err != nil {
		return fmt.Errorf("failed to init migrations table: %v", err)
	}

	// Define all migrations in order
	migrations := []Migration{
		{
			Name: "add_total_links_out_to_domains",
			MigrateFunc: func(db *sql.DB) error {
				log.Println("🔄 Migration: Adding total_links_out column to domains table...")

				// Add column
				_, err := db.Exec(`ALTER TABLE domains ADD COLUMN total_links_out INTEGER DEFAULT 0`)
				if err != nil {
					// Column might already exist, check if that's the error
					if err.Error() == "duplicate column name: total_links_out" {
						log.Println("   Column already exists, skipping ALTER TABLE")
					} else {
						return err
					}
				}

				// Backfill using temp table and bulk UPDATE FROM
				log.Println("   Creating temporary aggregation table...")
				_, err = db.Exec(`DROP TABLE IF EXISTS temp_link_counts`)
				if err != nil {
					return fmt.Errorf("failed to drop temp table: %v", err)
				}

				log.Println("   Calculating link counts (this may take a while)...")
				urlHostCol := "domain"
				if tableHasColumn(db, "urls", "host_domain") {
					urlHostCol = "host_domain"
				}
				domHostCol := "domain"
				if tableHasColumn(db, "domains", "host_domain") {
					domHostCol = "host_domain"
				}
				createTempQuery := fmt.Sprintf(`
					CREATE TEMP TABLE temp_link_counts AS
					SELECT 
						u1.%[1]s as domain,
						COUNT(DISTINCT u2.id) as link_count
					FROM urls u1
					INNER JOIN urls u2 ON u1.url = u2.from_url
					WHERE u1.status = 'fetched'
						AND u2.status != 'skipped'
						AND u1.%[1]s != u2.%[1]s
					GROUP BY u1.%[1]s
				`, urlHostCol)
				_, err = db.Exec(createTempQuery)
				if err != nil {
					return fmt.Errorf("failed to create temp table: %v", err)
				}

				log.Println("   Applying bulk update...")
				bulkUpdateQuery := fmt.Sprintf(`
					UPDATE domains
					SET total_links_out = (
						SELECT COALESCE(link_count, 0)
						FROM temp_link_counts
						WHERE temp_link_counts.domain = domains.%[1]s
					)
					WHERE EXISTS (
						SELECT 1 FROM temp_link_counts WHERE temp_link_counts.domain = domains.%[1]s
					)
				`, domHostCol)
				result, err := db.Exec(bulkUpdateQuery)
				if err != nil {
					return fmt.Errorf("bulk update failed: %v", err)
				}

				rowsAffected, _ := result.RowsAffected()
				log.Printf("   ✅ Backfilled link counts for %d domains\n", rowsAffected)

				return nil
			},
		},
		{
			Name: "add_total_urls_fetched_to_domains",
			MigrateFunc: func(db *sql.DB) error {
				log.Println("🔄 Migration: Adding total_urls_fetched column to domains table...")

				// Add column
				_, err := db.Exec(`ALTER TABLE domains ADD COLUMN total_urls_fetched INTEGER DEFAULT 0`)
				if err != nil {
					if err.Error() == "duplicate column name: total_urls_fetched" {
						log.Println("   Column already exists, skipping ALTER TABLE")
					} else {
						return err
					}
				}

				// Backfill using temp table for efficiency
				log.Println("   Creating temporary aggregation table...")
				_, err = db.Exec(`DROP TABLE IF EXISTS temp_url_counts`)
				if err != nil {
					return fmt.Errorf("failed to drop temp table: %v", err)
				}

				log.Println("   Calculating URL counts (this may take a while)...")
				urlHostCol := "domain"
				if tableHasColumn(db, "urls", "host_domain") {
					urlHostCol = "host_domain"
				}
				domHostCol := "domain"
				if tableHasColumn(db, "domains", "host_domain") {
					domHostCol = "host_domain"
				}
				createTempQuery := fmt.Sprintf(`
					CREATE TEMP TABLE temp_url_counts AS
					SELECT %[1]s as domain, COUNT(*) as url_count
					FROM urls
					WHERE status = 'fetched'
					GROUP BY %[1]s
				`, urlHostCol)
				_, err = db.Exec(createTempQuery)
				if err != nil {
					return fmt.Errorf("failed to create temp table: %v", err)
				}

				log.Println("   Applying bulk update...")
				bulkUpdateQuery := fmt.Sprintf(`
					UPDATE domains
					SET total_urls_fetched = (
						SELECT COALESCE(url_count, 0)
						FROM temp_url_counts
						WHERE temp_url_counts.domain = domains.%[1]s
					)
					WHERE EXISTS (
						SELECT 1 FROM temp_url_counts WHERE temp_url_counts.domain = domains.%[1]s
					)
				`, domHostCol)
				result, err := db.Exec(bulkUpdateQuery)
				if err != nil {
					return fmt.Errorf("bulk update failed: %v", err)
				}

				rowsAffected, _ := result.RowsAffected()
				log.Printf("   ✅ Backfilled URL counts for %d domains\n", rowsAffected)

				return nil
			},
		},
		{
			Name: "split_host_and_registrable_domain",
			MigrateFunc: func(db *sql.DB) error {
				return migrateHostRegistrableSplit(db)
			},
		},
	}

	// Run each migration if not already applied
	for _, migration := range migrations {
		applied, err := isMigrationApplied(db, migration.Name)
		if err != nil {
			return fmt.Errorf("failed to check migration %s: %v", migration.Name, err)
		}

		if applied {
			log.Printf("⏭️  Migration '%s' already applied, skipping\n", migration.Name)
			continue
		}

		log.Printf("▶️  Running migration: %s\n", migration.Name)
		startTime := time.Now()

		if err := migration.MigrateFunc(db); err != nil {
			return fmt.Errorf("migration %s failed: %v", migration.Name, err)
		}

		if err := markMigrationApplied(db, migration.Name); err != nil {
			return fmt.Errorf("failed to mark migration %s as applied: %v", migration.Name, err)
		}

		duration := time.Since(startTime)
		log.Printf("✅ Migration '%s' completed in %v\n", migration.Name, duration)
	}

	return nil
}

func backfillURLRegistrableDomains(db *sql.DB) error {
	rows, err := db.Query(`SELECT DISTINCT host_domain FROM urls WHERE host_domain IS NOT NULL AND host_domain != ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return err
		}
		site := registrableDomainFromHost(host)
		if _, err := db.Exec(`UPDATE urls SET domain = ? WHERE host_domain = ?`, site, host); err != nil {
			return err
		}
	}
	return rows.Err()
}

func backfillDomainRegistrable(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, host_domain FROM domains WHERE host_domain IS NOT NULL AND host_domain != ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var host string
		if err := rows.Scan(&id, &host); err != nil {
			return err
		}
		site := registrableDomainFromHost(host)
		if _, err := db.Exec(`UPDATE domains SET registrable_domain = ? WHERE id = ?`, site, id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func migrateHostRegistrableSplit(db *sql.DB) error {
	log.Println("🔄 Migration: Splitting host (crawl identity) vs registrable domain (site bucket)...")

	if !tableHasColumn(db, "urls", "host_domain") {
		log.Println("   Adding urls.host_domain and copying legacy host from urls.domain...")
		if _, err := db.Exec(`ALTER TABLE urls ADD COLUMN host_domain TEXT`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
		if _, err := db.Exec(`UPDATE urls SET host_domain = domain WHERE host_domain IS NULL OR host_domain = ''`); err != nil {
			return err
		}
	}

	if err := backfillURLRegistrableDomains(db); err != nil {
		return fmt.Errorf("backfill urls.domain (registrable): %w", err)
	}

	if tableHasColumn(db, "domains", "domain") {
		log.Println("   Renaming domains.domain -> domains.host_domain...")
		if _, err := db.Exec(`ALTER TABLE domains RENAME COLUMN domain TO host_domain`); err != nil {
			return fmt.Errorf("rename domains.domain: %w", err)
		}
	}

	if !tableHasColumn(db, "domains", "registrable_domain") {
		log.Println("   Adding domains.registrable_domain...")
		if _, err := db.Exec(`ALTER TABLE domains ADD COLUMN registrable_domain TEXT`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}

	if err := backfillDomainRegistrable(db); err != nil {
		return fmt.Errorf("backfill domains.registrable_domain: %w", err)
	}

	log.Println("   Clearing domain_links (will be rebuilt by analytics using registrable domains)...")
	if _, err := db.Exec(`DELETE FROM domain_links`); err != nil {
		return err
	}

	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_urls_host_domain ON urls(host_domain)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_urls_registrable ON urls(domain)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_domains_registrable ON domains(registrable_domain)`); err != nil {
		return err
	}

	log.Println("   ✅ Host vs registrable split complete")
	return nil
}
