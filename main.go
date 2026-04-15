package main

import (
	"bytes"
	"crypto/sha256"
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

	err = initDB(db)
	if err != nil {
		log.Fatal("Failed to initialize database:", err)
	}

	// Seed the database with starting URL if empty
	seedDatabaseIfEmpty(db)

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

		fmt.Printf("Starting %d workers...\n", numWorkers)

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

func initDB(db *sql.DB) error {
	query := `
	CREATE TABLE IF NOT EXISTS urls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT NOT NULL UNIQUE,
		domain TEXT NOT NULL,
		from_url TEXT,
		status TEXT DEFAULT 'pending',
		error TEXT,
		fetched_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	
	CREATE INDEX IF NOT EXISTS inx_status ON urls(status);
	CREATE INDEX IF NOT EXISTS idx_domain ON urls(domain);

	CREATE TABLE IF NOT EXISTS domains (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		domain TEXT NOT NULL UNIQUE,
		robots TEXT,
		robots_fetched TIMESTAMP,
		crawl_delay INTEGER DEFAULT 2,
		rate_limited BOOLEAN DEFAULT 0,
		rate_limit_reset TIMESTAMP,
		first_fetched TIMESTAMP,
		claimed_by INTEGER,
		claimed_at TIMESTAMP
	)
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

		fmt.Printf("Database empty, seeding with starting URL: %s\n", startingURL)

		// Get domain and fetch robots txt
		domain, err := resolveDomain(startingURL)
		if err != nil {
			log.Fatal("Failed to parse starting URL:", err)
		}

		// Fetch robots.txt for starting domain
		robotsTxt, err := fetchRobotsTXT(domain)
		if err != nil {
			log.Printf("Failed to fetch robots.txt for starting domain: %v", err)
			robotsTxt = ""
		}

		// Save domain
		saveDomain(db, domain, robotsTxt, time.Now())

		// Save starting URL
		_, err = db.Exec(`INSERT INTO urls (url, domain, status) VALUES (?, ?, 'pending')`, startingURL, domain)
		if err != nil {
			log.Fatal("Failed to seed database:", err)
		}

		fmt.Println("Database seeded successfully")
	}
}

func saveURL(db *sql.DB, urlStr string, fromURL string) error {
	domain, err := resolveDomain(urlStr)
	if err != nil {
		return err
	}

	// Check if this is a new domain - if so, register it
	var domainExists int
	err = db.QueryRow(`SELECT COUNT(*) FROM domains WHERE domain = ?`, domain).Scan(&domainExists)
	if err != nil {
		return err
	}

	if domainExists == 0 {
		// New domain discovered - fetch and save robots.txt
		fmt.Printf("New domain discovered: %s, fetching robots.txt...\n", domain)
		robotsTxt, err := fetchRobotsTXT(domain)
		if err != nil {
			log.Printf("Failed to fetch robots.txt for new domain %s: %v", domain, err)
			saveDomain(db, domain, "", time.Now()) // Save with empty robots
		} else {
			saveDomain(db, domain, robotsTxt, time.Now())
		}
	}

	_, err = db.Exec(`INSERT OR IGNORE INTO urls (url, domain, from_url) VALUES (?, ?, ?)`, urlStr, domain, fromURL)
	return err
}

func saveDomain(db *sql.DB, domain string, robots string, robotsFetched time.Time) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO domains (domain, robots, robots_fetched) VALUES (?, ?, ?)`, domain, robots, robotsFetched)
	return err
}

func resolveDomain(urlStr string) (string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}
	return u.Host, nil
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

func fetchRobotsTXT(domain string) (string, error) {
	// Create a new HTTP GET request
	req, err := http.NewRequest("GET", "https://"+domain+"/robots.txt", nil)
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

func getNextURLForDomain(db *sql.DB, domain string) (string, error) {
	var url string
	err := db.QueryRow(`
		SELECT url
		FROM urls
		WHERE domain = ? AND status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
	`, domain).Scan(&url)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no pending URLs for domain %s", domain)
	}
	return url, nil
}

func getNextDomainWithPendingURLs(db *sql.DB) (string, int, error) {
	var domain string
	var crawlDelay sql.NullInt64

	err := db.QueryRow(`
		SELECT d.domain, d.crawl_delay
		FROM domains d
		INNER JOIN urls u on d.domain = u.domain
		WHERE u.status = 'pending'
		GROUP BY d.domain
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
	_, err := db.Exec(`UPDATE urls SET status = 'fetched', fetched_at = ? WHERE url = ?`, time.Now(), urlStr)
	if err != nil {
		log.Printf("Failed to mark URL %s as fetched: %v", urlStr, err)
	}
}

func getDomainRobotsTXT(db *sql.DB, domain string) (string, error) {
	var robotsTXT string
	err := db.QueryRow(`
		SELECT robots FROM domains WHERE domain = ?
	`, domain).Scan(&robotsTXT)

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

func releaseDomain(db *sql.DB, domain string, workerID int) {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	// Check ownership
	if domainRegistry[domain] != workerID {
		log.Printf("Worker %d attempted to release domain, but doesn't own it", workerID)
		return
	}
	delete(domainRegistry, domain)

	// Notify DB that the domain is now free for other workers
	_, err := db.Exec(`
		UPDATE domains
		SET claimed_by = NULL, claimed_at = NULL
		WHERE domain = ? AND claimed_by = ?
	`, domain, workerID)

	if err != nil {
		log.Printf("Failed to relase domain %s by worker %d: %v", domain, workerID, err)
	} else {
		fmt.Printf("Worker %d successfully released domain %s\n", workerID, domain)
	}
}

func refreshClaim(db *sql.DB, domain string, workerID int) {
	_, err := db.Exec(`
		UPDATE domains
		SET claimed_at = CURRENT_TIMESTAMP
		WHERE domain = ? and claimed_by = ?
	`, domain, workerID)
	if err != nil {
		log.Printf("Worker %d failed to refresh claim on %s: %v", workerID, domain, err)
	}
}

func claimNextDomain(db *sql.DB, workerID int) (string, int, error) {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	var domain string
	var crawlDelay sql.NullInt64

	// Find unclaimed valid domain with pending work
	err := db.QueryRow(`
		SELECT d.domain, d.crawl_delay
		FROM domains d
		INNER JOIN urls u on d.domain = u.domain
		WHERE u.status = 'pending'
			AND (d.claimed_by IS NULL
				OR d.claimed_at < datetime("now", "-5 minutes"))
		GROUP BY d.domain
		ORDER BY MIN(u.created_at) ASC
		LIMIT 1
	`).Scan(&domain, &crawlDelay)

	if err == sql.ErrNoRows {
		return "", 0, fmt.Errorf("no available domains")
	}
	if err != nil {
		return "", 0, err
	}

	if existingWorker, exists := domainRegistry[domain]; exists {
		log.Printf("Domain %s claimed by worker %d in registry but not DB!", domain, existingWorker)
		return "", 0, fmt.Errorf("domain claimed by another worker")
	}

	// CLAIM in DB
	_, err = db.Exec(`
		UPDATE domains
		SET claimed_by = ?, claimed_at = CURRENT_TIMESTAMP
		WHERE domain = ?
	`, workerID, domain)

	if err != nil {
		return "", 0, err
	}

	// CLAIM in registry
	domainRegistry[domain] = workerID

	delay := 2
	if crawlDelay.Valid {
		delay = int(crawlDelay.Int64)
	}
	return domain, delay, nil
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
		// Attempt to claim the next available domain
		domain, crawlDelay, err := claimNextDomain(db, workerID)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		c.Printf("Worker %d claimed domain %s\n", workerID, domain)

		// Robots
		robotsTXT, err := getDomainRobotsTXT(db, domain)
		if err != nil || robotsTXT == "" {
			c.Printf("(WORKER_%d) Fetching robots.txt for %s\n", workerID, domain)
			robotsTXT, err = fetchRobotsTXT(domain)
			if err != nil {
				log.Printf("(WORKER_%d) Failed to fetch robots.txt for %s: %v", workerID, domain, err)
				robotsTXT = ""
			}
			saveDomain(db, domain, robotsTXT, time.Now())
		}

		// Process URLs
		processedCount := 0
		for {
			//Next URL
			urlStr, err := getNextURLForDomain(db, domain)
			if err != nil {
				c.Printf("(WORKER_%d) No more URLs to process for domain %s\n", workerID, domain)
				break
			}

			// Check domain against robots.txt
			allowed, _ := canCrawlURL(robotsTXT, urlStr)
			if !allowed {
				c.Printf("(WORKER_%d) URL %s disallowed by robots.txt for domain %s\n", workerID, urlStr, domain)
				markURLSkipped(db, urlStr, "disallowed by robots.txt")
				continue
			}

			// Fetch Url
			c.Printf("(WORKER_%d) Fetching URL %s for domain %s\n", workerID, urlStr, domain)
			resp, doc, bodyBytes, err := fetchURL(urlStr)
			if err != nil {
				log.Printf("(WORKER_%d) Failed to fetch URL %s for domain %s: %v", workerID, urlStr, domain, err)
				markURLFailed(db, urlStr, err.Error())
			} else {
				links := parseLinks(doc, urlStr)
				for _, link := range links {
					saveURL(db, link, urlStr)
				}
				markURLFetched(db, urlStr)

				// save to disk
				if os.Getenv("SAVE_DATA") == "true" {
					err = saveBodyToDisk(urlStr, bodyBytes)
					if err != nil {
						log.Printf("(WORKER_%d) Failed to save body for URL %s: %v", workerID, urlStr, err)
					}
				}

				processedCount++

				refreshClaim(db, domain, workerID)

				c.Printf("(WORKER_%d) STATUS %d, Links found: %d\n", workerID, resp.StatusCode, len(links))
			}
			// Respect crawl delay
			time.Sleep(time.Duration(crawlDelay) * time.Second)
		}
		releaseDomain(db, domain, workerID)
	}
}
