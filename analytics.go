package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// AnalyticsService handles periodic analysis of domain link relationships
type AnalyticsService struct {
	db        *sql.DB
	frequency time.Duration
	enabled   bool
}

type topDomainRow struct {
	Registerable string
	LinksOut     int64
	PagesFetched int64
}

// NewAnalyticsService creates a new analytics service
func NewAnalyticsService(db *sql.DB, frequencySeconds int, enabled bool) *AnalyticsService {
	return &AnalyticsService{
		db:        db,
		frequency: time.Duration(frequencySeconds) * time.Second,
		enabled:   enabled,
	}
}

// Start begins the analytics service in a goroutine
func (a *AnalyticsService) Start() {
	if !a.enabled {
		log.Println("Analytics service is disabled")
		return
	}

	log.Printf("Analytics service started with %v frequency\n", a.frequency)
	go a.run()
}

// run is the main loop for the analytics service
func (a *AnalyticsService) run() {
	// Run immediately on startup
	a.analyzeDomainLinks()

	// Then run periodically
	ticker := time.NewTicker(a.frequency)
	defer ticker.Stop()

	for range ticker.C {
		a.analyzeDomainLinks()
	}
}

// analyzeDomainLinks calculates and updates domain link relationships
// Optimized to only process top 100 domains by fetched URL count
func (a *AnalyticsService) analyzeDomainLinks() {
	startTime := time.Now()
	fmt.Println("🔍 Analytics: Starting domain link analysis (top 100 domains)...")

	// Fetch top 250 domains by fetched URL count
	topDomains, err := fetchTopDomains(a.db)
	if err != nil {
		log.Printf("Analytics: Error fetching top domains: %v", err)
		return
	}
	// Clear old top domain entries
	err = clearOldTopDomains(a.db)
	if err != nil {
		log.Printf("Analytics: Error clearing old top domains: %v", err)
		return
	}

	// Repopulate top domain entries
	err = populateTopDomains(a.db, topDomains)
	if err != nil {
		log.Printf("Analytics: Error populating top domains: %v", err)
		return
	}
	log.Printf("Analytics: domain link analysis done in %v (%d rows)\n", time.Since(startTime), len(topDomains))
}

func fetchTopDomains(db *sql.DB) ([]topDomainRow, error) {
	query := `
		SELECT
			registrable_domain,
			SUM(total_links_out)   AS links_out_across_hosts,
			SUM(total_urls_fetched) AS pages_fetched_across_hosts
		FROM domains
		GROUP BY registrable_domain
		HAVING SUM(total_urls_fetched) > 0 OR SUM(total_links_out) > 0
		ORDER BY SUM(total_urls_fetched) DESC
		LIMIT 250
	`
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []topDomainRow
	for rows.Next() {
		var r topDomainRow

		if err := rows.Scan(&r.Registerable, &r.LinksOut, &r.PagesFetched); err != nil {
			return nil, err
		}

		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error scanning rows: %w", err)
	}

	return out, nil
}

func clearOldTopDomains(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM domain_top`)
	return err
}

func populateTopDomains(db *sql.DB, topDomains []topDomainRow) error {
	for i, d := range topDomains {
		pos := i + 1
		_, err := db.Exec(`
			INSERT INTO domain_top 
				(position, domain, total_links_fetched) 
			VALUES (?, ?, ?)
			`, pos, d.Registerable, d.LinksOut)
		if err != nil {
			return err
		}
	}
	return nil
}
