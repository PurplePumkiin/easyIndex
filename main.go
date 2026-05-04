package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"database/sql"

	_ "github.com/mattn/go-sqlite3"

	"github.com/PuerkitoBio/goquery"
	"github.com/fatih/color"
	"github.com/joho/godotenv"
	"github.com/temoto/robotstxt"
)

var domainRegistry map[string]int
var registryMutex sync.Mutex
var numWorkers int
var maxWorkersPerSite int

func main() {
	// Load environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: .env file not found, proceeding with environment variables")
	}

	// Initialize directories
	initDir()

	// Initialize database
	db, err := sql.Open("sqlite3", "./db/indexer.db")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}
	db.Exec(`PRAGMA journal_mode=WAL`)
	defer db.Close()

	err = freshStartIfNeeded(db)
	if err != nil {
		log.Fatal("Fresh-start database reset:", err)
	}

	err = initDB(db)
	if err != nil {
		log.Fatal("Failed to initialize database:", err)
	}

	// Seed the database with starting URL if empty
	seedDatabaseIfEmpty(db)

	// Start Analytics Service
	analyticsEnabled := os.Getenv("ANALYTICS") == "true"
	analyticsFrequency := getEnvInt("FREQUENCY", 3600) // Default 1 hour
	analyticsService := NewAnalyticsService(db, analyticsFrequency, analyticsEnabled)
	analyticsService.Start()

	// Main crawl loop
	for {
		// Initialize registry
		domainRegistry = make(map[string]int)

		// Worker count
		numWorkersStr := os.Getenv("NUM_WORKERS")
		if numWorkersStr == "" {
			numWorkers = 10
		} else {
			var err error
			numWorkers, err = strconv.Atoi(numWorkersStr)
			if err != nil {
				log.Printf("Invalid NUM_WORKERS value: %s", numWorkersStr)
				numWorkers = 10
			}
		}

		maxWorkersPerSite = getEnvInt("MAX_WORKERS_PER_SITE", 5)
		if maxWorkersPerSite < 1 {
			maxWorkersPerSite = 1
		}

		fmt.Printf("Starting %d workers (max %d per registrable site)...\n", numWorkers, maxWorkersPerSite)

		// Create the WaitGroup to track workers
		var wg sync.WaitGroup

		//Start worker goroutines
		for i := 1; i <= numWorkers; i++ {
			wg.Add(1)
			go worker(i, db, &wg)
		}

		// Should never see this
		wg.Wait()
		fmt.Println("All workers somehow done")
	}
}

// getEnvInt gets an integer from environment variable with a default value
func getEnvInt(key string, defaultValue int) int {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultValue
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		log.Printf("Invalid %s value: %s, using default: %d", key, valStr, defaultValue)
		return defaultValue
	}
	return val
}

func fetchURL(url string) (*http.Response, *goquery.Document, []byte, error) {
	// Create a new HTTP GET request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	// Load in headers
	req.Header.Set("User-Agent", "GOindexer/1.0")

	// Execute request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil, nil, fmt.Errorf("non-200 status code: %d", resp.StatusCode)
	}

	// Move body to bytes
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, nil, err
	}

	// Create doc from the bytes
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, nil, err
	}

	return resp, doc, bodyBytes, nil
}

func parseLinks(doc *goquery.Document, baseURL string) []string {
	var links []string
	// Find all anchor tags and extract the href attribute
	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")

		absoluteURL, err := resolveURL(baseURL, href)
		if err != nil {
			return
		}

		if !isValidURL(absoluteURL) {
			return
		}
		links = append(links, absoluteURL)
	})
	return links
}

func initDir() {
	err := os.MkdirAll("./db", 0755)
	if err != nil {
		log.Fatal("Failed to create db directory:", err)
	}

	err = os.MkdirAll("./data", 0755)
	if err != nil {
		log.Fatal("Failed to create data directory:", err)
	}
}

// freshStartAlreadyApplied is true when this database has already completed the one-time nuclear reset.
func freshStartAlreadyApplied(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migrations'`).Scan(&n)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	err = db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE name = 'fresh_start'`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// dropAllUserTables removes every non-internal SQLite table so we can recreate a clean schema.
func dropAllUserTables(db *sql.DB) (err error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		if _, e := db.Exec(`PRAGMA foreign_keys = ON`); e != nil {
			err = errors.Join(err, fmt.Errorf("PRAGMA foreign_keys=ON: %w", e))
		}
	}()

	for _, name := range names {
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if _, e := db.Exec("DROP TABLE IF EXISTS " + quoted); e != nil {
			return e
		}
	}
	return nil
}

// freshStartIfNeeded runs once per database file: drops every table, recreates empty urls/domains/migrations,
// and records the fresh_start sentinel. Later startups see the row and skip this entirely.
func freshStartIfNeeded(db *sql.DB) error {
	done, err := freshStartAlreadyApplied(db)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	log.Println("Database: one-time fresh_start — dropping all tables and recreating schema")

	if err := dropAllUserTables(db); err != nil {
		return err
	}
	if err := initDB(db); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO migrations (id, name, applied_at) VALUES (1, 'fresh_start', ?)`, time.Now())
	return err
}

// initDB ensures all application tables and indexes exist (domain_links and analytics-related objects live here).
func initDB(db *sql.DB) error {
	query := `
	CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS urls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT NOT NULL UNIQUE,
		host_domain TEXT NOT NULL,
		domain TEXT NOT NULL,
		from_url TEXT,
		from_domain TEXT,
		status TEXT DEFAULT 'pending',
		error TEXT,
		fetched_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	
	CREATE INDEX IF NOT EXISTS inx_status ON urls(status);
	CREATE INDEX IF NOT EXISTS idx_urls_host_domain ON urls(host_domain);
	CREATE INDEX IF NOT EXISTS idx_urls_registrable ON urls(domain);

	CREATE TABLE IF NOT EXISTS domains (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host_domain TEXT NOT NULL UNIQUE,
		registrable_domain TEXT NOT NULL,
		robots TEXT,
		robots_fetched TIMESTAMP,
		crawl_delay INTEGER DEFAULT 2,
		rate_limited BOOLEAN DEFAULT 0,
		rate_limit_reset TIMESTAMP,
		first_fetched TIMESTAMP,
		claimed_by INTEGER,
		claimed_at TIMESTAMP,
		total_links_out INTEGER DEFAULT 0,
		total_urls_fetched INTEGER DEFAULT 0
	);
	
	CREATE TABLE IF NOT EXISTS domain_top (
		position INTEGER PRIMARY KEY NOT NULL,
		domain TEXT NOT NULL,
		total_links_fetched INTEGER DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS domain_links (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_domain TEXT NOT NULL,
		target_domain TEXT NOT NULL,
		link_count INTEGER DEFAULT 0,
		last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(source_domain, target_domain)
	);

	CREATE INDEX IF NOT EXISTS idx_source_domain ON domain_links(source_domain);
	CREATE INDEX IF NOT EXISTS idx_link_count ON domain_links(link_count DESC);

	CREATE TABLE IF NOT EXISTS analytics_worker (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		links_proccessed INTEGER DEFAULT 0,
		time_taken_seconds INTEGER DEFAULT 0,
		last_run TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		ending_id INTEGER DEFAULT 0
	);
	`

	_, err := db.Exec(query)
	return err
}

func seedDatabaseIfEmpty(db *sql.DB) {
	// Check if we have any URLs in the database
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM urls`).Scan(&count)
	if err != nil {
		log.Fatal("Failed to check database:", err)
	}

	if count == 0 {
		// Database is empty, seed with starting URL
		startingURL := os.Getenv("STARTING_URL")
		if startingURL == "" {
			log.Fatal("Database is empty and STARTING_URL not set in .env")
		}

		startingURL, err = normalizeURLString(startingURL)
		if err != nil {
			log.Fatal("Failed to normalize STARTING_URL:", err)
		}

		fmt.Printf("Database empty, seeding with starting URL: %s\n", startingURL)

		host, site, err := resolveHostAndSite(startingURL)
		if err != nil {
			log.Fatal("Failed to parse starting URL:", err)
		}

		// Fetch robots.txt for starting host
		robotsTxt, err := fetchRobotsTXT(host)
		if err != nil {
			log.Printf("Failed to fetch robots.txt for starting host: %v", err)
			robotsTxt = ""
		}

		saveDomain(db, host, site, robotsTxt, time.Now())

		_, err = db.Exec(`INSERT INTO urls (url, host_domain, domain, status) VALUES (?, ?, ?, 'pending')`, startingURL, host, site)
		if err != nil {
			log.Fatal("Failed to seed database:", err)
		}

		fmt.Println("Database seeded successfully")
	}
}

func saveURL(db *sql.DB, urlStr string, fromURL string) error {
	var err error
	urlStr, err = normalizeURLString(urlStr)
	if err != nil {
		return err
	}
	if fromURL != "" {
		fromURL, err = normalizeURLString(fromURL)
		if err != nil {
			return err
		}
	}

	host, site, err := resolveHostAndSite(urlStr)
	if err != nil {
		return err
	}

	var hostExists int
	err = db.QueryRow(`SELECT COUNT(*) FROM domains WHERE host_domain = ?`, host).Scan(&hostExists)
	if err != nil {
		return err
	}

	if hostExists == 0 {
		fmt.Printf("New host discovered: %s (site %s), fetching robots.txt...\n", host, site)
		robotsTxt, err := fetchRobotsTXT(host)
		if err != nil {
			log.Printf("Failed to fetch robots.txt for new host %s: %v", host, err)
			saveDomain(db, host, site, "", time.Now())
		} else {
			saveDomain(db, host, site, robotsTxt, time.Now())
		}
	}

	fromDomain := ""
	if fromURL != "" {
		fromDomain, err = hostFromURLString(fromURL)
		if err != nil {
			return err
		}
		fromDomain = registrableDomainFromHost(fromDomain)
	}

	result, err := db.Exec(`INSERT OR IGNORE INTO urls (url, host_domain, domain, from_url, from_domain) VALUES (?, ?, ?, ?, ?)`, urlStr, host, site, fromURL, fromDomain)
	if err != nil {
		return err
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected > 0 && fromURL != "" {
		sourceHost, sourceSite, err1 := resolveHostAndSite(fromURL)
		if err1 == nil && sourceSite != site {
			_, err = db.Exec(`
				UPDATE domains 
				SET total_links_out = total_links_out + 1 
				WHERE host_domain = ?
			`, sourceHost)
			if err != nil {
				log.Printf("Warning: failed to increment link counter for %s: %v", sourceHost, err)
			}
		}
	}

	return nil
}

func saveDomain(db *sql.DB, host string, site string, robots string, robotsFetched time.Time) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO domains (host_domain, registrable_domain, robots, robots_fetched) VALUES (?, ?, ?, ?)`, host, site, robots, robotsFetched)
	return err
}

func resolveURL(baseURL, href string) (string, error) {
	// Parse base
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	// Parse href
	ref, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	// Resolve reference against base
	resolved := base.ResolveReference(ref)
	return resolved.String(), nil
}

func isValidURL(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Scheme != "" && u.Host != ""
}

func fetchRobotsTXT(host string) (string, error) {
	// Create a new HTTP GET request
	req, err := http.NewRequest("GET", "https://"+host+"/robots.txt", nil)
	if err != nil {
		return "", err
	}
	// Load in headers
	req.Header.Set("User-Agent", "GOindexer/1.0")

	// Execute request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("non-200 status code: %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(bodyBytes), nil
}

func getNextURLForDomain(db *sql.DB, host string) (string, error) {
	var url string
	err := db.QueryRow(`
		SELECT url
		FROM urls
		WHERE host_domain = ? AND status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
	`, host).Scan(&url)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no pending URLs for host %s", host)
	}
	return url, nil
}

func getNextDomainWithPendingURLs(db *sql.DB) (string, int, error) {
	var domain string
	var crawlDelay sql.NullInt64

	err := db.QueryRow(`
		SELECT d.host_domain, d.crawl_delay
		FROM domains d
		INNER JOIN urls u ON d.host_domain = u.host_domain
		WHERE u.status = 'pending'
		GROUP BY d.host_domain
		ORDER BY MIN(u.created_at) ASC
		LIMIT 1
	`).Scan(&domain, &crawlDelay)

	if err == sql.ErrNoRows {
		return "", 0, fmt.Errorf("no domains with pending URLs found")
	}
	delay := 2
	if crawlDelay.Valid {
		delay = int(crawlDelay.Int64)
	}
	return domain, delay, err
}

func markURLFailed(db *sql.DB, urlStr string, errMsg string) {
	_, err := db.Exec(`UPDATE urls SET status = 'failed', error = ?, fetched_at = ? WHERE url = ?`, errMsg, time.Now(), urlStr)
	if err != nil {
		log.Printf("Failed to mark URL %s as failed: %v", urlStr, err)
	}
}

func markURLFetched(db *sql.DB, urlStr string) {
	host, herr := hostFromURLString(urlStr)
	if herr != nil {
		host = ""
	}

	_, err := db.Exec(`UPDATE urls SET status = 'fetched', fetched_at = ? WHERE url = ?`, time.Now(), urlStr)
	if err != nil {
		log.Printf("Failed to mark URL %s as fetched: %v", urlStr, err)
		return
	}

	if host != "" {
		_, err = db.Exec(`
			UPDATE domains 
			SET total_urls_fetched = total_urls_fetched + 1 
			WHERE host_domain = ?
		`, host)
		if err != nil {
			log.Printf("Warning: failed to increment fetch counter for %s: %v", host, err)
		}
	}
}

func getDomainRobotsTXT(db *sql.DB, host string) (string, error) {
	var robotsTXT string
	err := db.QueryRow(`
		SELECT robots FROM domains WHERE host_domain = ?
	`, host).Scan(&robotsTXT)

	if err != nil {
		return "", nil
	}
	return robotsTXT, nil
}

func canCrawlURL(robotsTXT string, urlStr string) (bool, int) {
	if robotsTXT == "" {
		return true, 0
	}

	robots, err := robotstxt.FromString(robotsTXT)
	if err != nil {
		return true, 0
	}

	// Check if our user-agent is allowed to crawl this URL
	group := robots.FindGroup("GOindexer/1.0")
	allowed := group.Test(urlStr)

	// Get the crawl delay
	crawlDelay := int(group.CrawlDelay.Seconds())

	return allowed, crawlDelay
}

func markURLSkipped(db *sql.DB, urlStr string, reason string) error {
	_, err := db.Exec(`
		UPDATE urls
		SET status = 'skipped', error = ?
		WHERE url = ?
	`, reason, urlStr)
	return err
}

func saveBodyToDisk(urlStr string, body []byte) error {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return err
	}

	// Create a safe file path based on the hashed URL
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(urlStr)))
	filePath := "data/" + parsedURL.Host + "/" + hash + ".html"
	if strings.HasSuffix(filePath, "/") {
		filePath += "index.html"
	}

	// Ensure the directory exists
	dir := path.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Write the body to disk
	return os.WriteFile(filePath, body, 0644)
}

func releaseDomain(db *sql.DB, host string, workerID int) {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	if domainRegistry[host] != workerID {
		log.Printf("Worker %d attempted to release host claim, but doesn't own it", workerID)
		return
	}
	delete(domainRegistry, host)

	_, err := db.Exec(`
		UPDATE domains
		SET claimed_by = NULL, claimed_at = NULL
		WHERE host_domain = ? AND claimed_by = ?
	`, host, workerID)

	if err != nil {
		log.Printf("Failed to relase host %s by worker %d: %v", host, workerID, err)
	} else {
		fmt.Printf("Worker %d successfully released host %s\n", workerID, host)
	}
}

func refreshClaim(db *sql.DB, host string, workerID int) {
	_, err := db.Exec(`
		UPDATE domains
		SET claimed_at = CURRENT_TIMESTAMP
		WHERE host_domain = ? AND claimed_by = ?
	`, host, workerID)
	if err != nil {
		log.Printf("Worker %d failed to refresh claim on %s: %v", workerID, host, err)
	}
}

func claimNextDomain(db *sql.DB, workerID int) (string, int, error) {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	var host string
	var crawlDelay sql.NullInt64

	err := db.QueryRow(`
		SELECT d.host_domain, d.crawl_delay
		FROM domains d
		INNER JOIN urls u ON d.host_domain = u.host_domain
		WHERE u.status = 'pending'
			AND (d.claimed_by IS NULL
				OR d.claimed_at < datetime('now', '-5 minutes'))
			AND (
				SELECT COUNT(DISTINCT cx.claimed_by)
				FROM domains cx
				WHERE cx.registrable_domain = d.registrable_domain
					AND cx.claimed_by IS NOT NULL
					AND cx.claimed_at >= datetime('now', '-5 minutes')
			) < ?
		GROUP BY d.host_domain
		ORDER BY MIN(u.created_at) ASC
		LIMIT 1
	`, maxWorkersPerSite).Scan(&host, &crawlDelay)

	if err == sql.ErrNoRows {
		return "", 0, fmt.Errorf("no available domains")
	}
	if err != nil {
		return "", 0, err
	}

	if existingWorker, exists := domainRegistry[host]; exists {
		log.Printf("Host %s claimed by worker %d in registry but not DB!", host, existingWorker)
		return "", 0, fmt.Errorf("host claimed by another worker")
	}

	_, err = db.Exec(`
		UPDATE domains
		SET claimed_by = ?, claimed_at = CURRENT_TIMESTAMP
		WHERE host_domain = ?
	`, workerID, host)

	if err != nil {
		return "", 0, err
	}

	domainRegistry[host] = workerID

	delay := 2
	if crawlDelay.Valid {
		delay = int(crawlDelay.Int64)
	}
	return host, delay, nil
}

func getWorkerColor() *color.Color {
	colors := []*color.Color{
		color.New(color.FgCyan),
		color.New(color.FgGreen),
		color.New(color.FgYellow),
		color.New(color.FgBlue),
		color.New(color.FgMagenta),
		color.New(color.FgRed),
		color.New(color.FgHiCyan),
		color.New(color.FgHiGreen),
		color.New(color.FgHiYellow),
		color.New(color.FgHiBlue),
		color.New(color.FgHiMagenta),
		color.New(color.FgHiRed),
	}
	return colors[rand.Intn(len(colors))]
}

func worker(workerID int, db *sql.DB, wg *sync.WaitGroup) {
	defer wg.Done()

	// Randomly assign color to this worker
	c := getWorkerColor()
	c.Printf("Worker %d started\n", workerID)

	for {
		host, crawlDelay, err := claimNextDomain(db, workerID)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		site := registrableDomainFromHost(host)
		c.Printf("Worker %d claimed host %s (site %s)\n", workerID, host, site)

		robotsTXT, err := getDomainRobotsTXT(db, host)
		if err != nil || robotsTXT == "" {
			c.Printf("(WORKER_%d) Fetching robots.txt for %s\n", workerID, host)
			robotsTxt, err := fetchRobotsTXT(host)
			if err != nil {
				log.Printf("(WORKER_%d) Failed to fetch robots.txt for %s: %v", workerID, host, err)
				robotsTxt = ""
			}
			saveDomain(db, host, site, robotsTxt, time.Now())
		}

		for {
			urlStr, err := getNextURLForDomain(db, host)
			if err != nil {
				c.Printf("(WORKER_%d) No more URLs to process for host %s\n", workerID, host)
				break
			}

			allowed, _ := canCrawlURL(robotsTXT, urlStr)
			if !allowed {
				c.Printf("(WORKER_%d) URL %s disallowed by robots.txt for host %s\n", workerID, urlStr, host)
				markURLSkipped(db, urlStr, "disallowed by robots.txt")
				continue
			}

			c.Printf("(WORKER_%d) Fetching URL %s (host %s)\n", workerID, urlStr, host)
			resp, doc, bodyBytes, err := fetchURL(urlStr)
			if err != nil {
				log.Printf("(WORKER_%d) Failed to fetch URL %s for host %s: %v", workerID, urlStr, host, err)
				markURLFailed(db, urlStr, err.Error())
			} else {
				links := parseLinks(doc, urlStr)
				for _, link := range links {
					saveURL(db, link, urlStr)
				}
				markURLFetched(db, urlStr)

				if os.Getenv("SAVE_DATA") == "true" {
					err = saveBodyToDisk(urlStr, bodyBytes)
					if err != nil {
						log.Printf("(WORKER_%d) Failed to save body for URL %s: %v", workerID, urlStr, err)
					}
				}

				refreshClaim(db, host, workerID)

				c.Printf("(WORKER_%d) STATUS %d, Links found: %d\n", workerID, resp.StatusCode, len(links))
			}
			time.Sleep(time.Duration(crawlDelay) * time.Second)
		}
		releaseDomain(db, host, workerID)
	}
}
