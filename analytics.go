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

	// Top 100 registrable domains (sites) by total fetched URLs across all hosts
	topDomainsQuery := `
		SELECT registrable_domain
		FROM domains
		GROUP BY registrable_domain
		HAVING SUM(total_urls_fetched) > 0
		ORDER BY SUM(total_urls_fetched) DESC
		LIMIT 100
	`

	rows, err := a.db.Query(topDomainsQuery)
	if err != nil {
		log.Printf("Analytics error querying top domains: %v", err)
		return
	}

	var topDomains []string
	for rows.Next() {
		var domain string
		if err := rows.Scan(&domain); err != nil {
			log.Printf("Analytics error scanning domain: %v", err)
			continue
		}
		topDomains = append(topDomains, domain)
	}
	rows.Close()

	if len(topDomains) == 0 {
		fmt.Println("⏭️  Analytics: No domains with fetched URLs yet")
		return
	}

	fmt.Printf("📊 Analytics: Processing %d top domains...\n", len(topDomains))

	// Build a comma-separated list of top domains for SQL IN clause
	var domainList string
	for i, domain := range topDomains {
		if i > 0 {
			domainList += ", "
		}
		domainList += "'" + domain + "'"
	}

	// Compute all domain link relationships in a single bulk query using temp table
	fmt.Println("   Creating temporary link aggregation table...")
	tx, err := a.db.Begin()
	if err != nil {
		log.Printf("Analytics error starting transaction: %v", err)
		return
	}

	_, err = tx.Exec(`DROP TABLE IF EXISTS temp_domain_links`)
	if err != nil {
		tx.Rollback()
		log.Printf("Analytics error dropping temp table: %v", err)
		return
	}

	fmt.Println("   Calculating domain link relationships (single pass)...")
	bulkLinkQuery := `
		CREATE TEMP TABLE temp_domain_links AS
		SELECT 
			u1.domain as source_domain,
			u2.domain as target_domain,
			COUNT(*) as link_count
		FROM urls u1
		INNER JOIN urls u2 ON u1.url = u2.from_url
		WHERE u1.domain IN (` + domainList + `)
			AND u1.status = 'fetched' 
			AND u2.status != 'skipped'
			AND u1.domain != u2.domain
		GROUP BY u1.domain, u2.domain
	`

	_, err = tx.Exec(bulkLinkQuery)
	if err != nil {
		tx.Rollback()
		log.Printf("Analytics error creating temp link table: %v", err)
		return
	}

	fmt.Println("   Updating domain_links table from temp table...")
	updateQuery := `
		INSERT INTO domain_links (source_domain, target_domain, link_count, last_updated)
		SELECT source_domain, target_domain, link_count, CURRENT_TIMESTAMP
		FROM temp_domain_links
		ON CONFLICT(source_domain, target_domain) 
		DO UPDATE SET 
			link_count = excluded.link_count,
			last_updated = CURRENT_TIMESTAMP
	`

	result, err := tx.Exec(updateQuery)
	if err != nil {
		tx.Rollback()
		log.Printf("Analytics error updating domain_links: %v", err)
		return
	}

	linksProcessed, _ := result.RowsAffected()

	if err := tx.Commit(); err != nil {
		log.Printf("Analytics error committing transaction: %v", err)
		return
	}

	duration := time.Since(startTime)
	fmt.Printf("✅ Analytics: Processed %d domain link relationships in %v\n", linksProcessed, duration)
}

// GetTotalURLs returns the total number of URLs scraped
func GetTotalURLs(db *sql.DB) (int, error) {
	var total int
	err := db.QueryRow(`SELECT COUNT(*) FROM urls WHERE status = 'fetched'`).Scan(&total)
	return total, err
}

// GetTotalDomains returns the number of distinct registrable domains (sites) discovered
func GetTotalDomains(db *sql.DB) (int, error) {
	var total int
	err := db.QueryRow(`SELECT COUNT(DISTINCT registrable_domain) FROM domains`).Scan(&total)
	return total, err
}

// DomainStats represents statistics for a single domain
type DomainStats struct {
	Domain         string `json:"domain"`
	URLsDiscovered int    `json:"urls_discovered"`
	URLsFetched    int    `json:"urls_fetched"`
	OutgoingLinks  int    `json:"outgoing_links"`
	IncomingLinks  int    `json:"incoming_links"`
	UniqueTargets  int    `json:"unique_targets"`
}

// GetDomainStats returns statistics for all domains
// GetDomainStats returns statistics for all domains (optimized with cached columns)
func GetDomainStats(db *sql.DB) ([]DomainStats, error) {
	query := `
		WITH site_totals AS (
			SELECT
				registrable_domain,
				SUM(COALESCE(total_urls_fetched, 0)) AS urls_fetched,
				SUM(COALESCE(total_links_out, 0)) AS outgoing_links
			FROM domains
			GROUP BY registrable_domain
		),
		discovered AS (
			SELECT
				domain AS registrable_domain,
				COUNT(DISTINCT url) AS urls_discovered
			FROM urls
			GROUP BY domain
		),
		outgoing AS (
			SELECT source_domain, COUNT(DISTINCT target_domain) AS unique_targets
			FROM domain_links
			GROUP BY source_domain
		),
		incoming AS (
			SELECT target_domain, SUM(link_count) AS link_count
			FROM domain_links
			GROUP BY target_domain
		)
		SELECT 
			s.registrable_domain,
			COALESCE(disc.urls_discovered, 0) as urls_discovered,
			COALESCE(st.urls_fetched, 0) as urls_fetched,
			COALESCE(st.outgoing_links, 0) as outgoing_links,
			COALESCE(incoming.link_count, 0) as incoming_links,
			COALESCE(outgoing.unique_targets, 0) as unique_targets
		FROM (SELECT DISTINCT registrable_domain FROM domains) s
		LEFT JOIN site_totals st ON st.registrable_domain = s.registrable_domain
		LEFT JOIN discovered disc ON disc.registrable_domain = s.registrable_domain
		LEFT JOIN outgoing ON s.registrable_domain = outgoing.source_domain
		LEFT JOIN incoming ON s.registrable_domain = incoming.target_domain
		ORDER BY urls_fetched DESC
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []DomainStats
	for rows.Next() {
		var s DomainStats
		err := rows.Scan(&s.Domain, &s.URLsDiscovered, &s.URLsFetched, &s.OutgoingLinks, &s.IncomingLinks, &s.UniqueTargets)
		if err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}

	return stats, nil
}

// DomainLink represents a link from one domain to another
type DomainLink struct {
	TargetDomain string `json:"target_domain"`
	LinkCount    int    `json:"link_count"`
}

// GetTopLinksForDomain returns the top N domains that a source domain links to
func GetTopLinksForDomain(db *sql.DB, sourceDomain string, limit int) ([]DomainLink, error) {
	query := `
		SELECT target_domain, link_count
		FROM domain_links
		WHERE source_domain = ?
		ORDER BY link_count DESC
		LIMIT ?
	`

	rows, err := db.Query(query, sourceDomain, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []DomainLink
	for rows.Next() {
		var link DomainLink
		err := rows.Scan(&link.TargetDomain, &link.LinkCount)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}

	return links, nil
}

// GraphData represents the complete network graph for visualization
type GraphData struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphNode represents a domain node in the visualization
type GraphNode struct {
	ID        string `json:"id"`
	Domain    string `json:"domain"`
	URLCount  int    `json:"url_count"`
	LinkCount int    `json:"link_count"`
}

// GraphEdge represents a connection between domains
type GraphEdge struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	LinkCount int    `json:"link_count"`
}

// GetGraphData returns all nodes and edges for network visualization
// GetGraphData returns all nodes and edges for network visualization (optimized)
func GetGraphData(db *sql.DB, minLinks int) (*GraphData, error) {
	// Get nodes (domains with their stats) - using cached columns for speed
	nodeQuery := `
		SELECT 
			d.registrable_domain,
			SUM(COALESCE(d.total_urls_fetched, 0)) as url_count,
			SUM(COALESCE(d.total_links_out, 0)) as total_links
		FROM domains d
		GROUP BY d.registrable_domain
		HAVING SUM(COALESCE(d.total_links_out, 0)) >= ?
		ORDER BY SUM(COALESCE(d.total_links_out, 0)) DESC
	`

	rows, err := db.Query(nodeQuery, minLinks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := []GraphNode{}
	domainSet := make(map[string]bool)

	for rows.Next() {
		var node GraphNode
		err := rows.Scan(&node.Domain, &node.URLCount, &node.LinkCount)
		if err != nil {
			return nil, err
		}
		node.ID = node.Domain
		nodes = append(nodes, node)
		domainSet[node.Domain] = true
	}

	// Get edges (links between domains)
	edgeQuery := `
		SELECT source_domain, target_domain, link_count
		FROM domain_links
		WHERE link_count >= ?
		ORDER BY link_count DESC
	`

	edgeRows, err := db.Query(edgeQuery, minLinks)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()

	edges := []GraphEdge{}
	for edgeRows.Next() {
		var edge GraphEdge
		err := edgeRows.Scan(&edge.Source, &edge.Target, &edge.LinkCount)
		if err != nil {
			return nil, err
		}
		// Only include edges where both source and target are in our node set
		if domainSet[edge.Source] && domainSet[edge.Target] {
			edges = append(edges, edge)
		}
	}

	return &GraphData{
		Nodes: nodes,
		Edges: edges,
	}, nil
}
