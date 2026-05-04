package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/fatih/color"
)

// APIService handles both GET (server) and POST (client) modes
type APIService struct {
	db          *sql.DB
	mode        string // "GET" or "POST"
	port        string
	destination string
	frequency   time.Duration
	enabled     bool
}

// NewAPIService creates a new API service
func NewAPIService(db *sql.DB, enabled bool, mode string, port string, destination string, frequencySeconds int) *APIService {
	return &APIService{
		db:          db,
		enabled:     enabled,
		mode:        mode,
		port:        port,
		destination: destination,
		frequency:   time.Duration(frequencySeconds) * time.Second,
	}
}

// Start begins the API service based on its mode
func (api *APIService) Start() {
	if !api.enabled {
		log.Println("API service is disabled")
		return
	}

	switch api.mode {
	case "GET":
		log.Printf("Starting API server on port %s\n", api.port)
		go api.startServer()
	case "POST":
		log.Printf("Starting API client - will POST to %s every %v\n", api.destination, api.frequency)
		go api.startClient()
	default:
		log.Printf("Invalid API mode: %s (must be GET or POST)", api.mode)
	}
}

// startServer starts the HTTP server for GET mode
func (api *APIService) startServer() {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"service": "GOindexer API",
			"version": "1.0",
		})
	})

	// Total stats endpoint
	mux.HandleFunc("/stats", api.handleStats)

	// Domain statistics endpoint
	mux.HandleFunc("/domains", api.handleDomains)

	// Top links for a specific domain
	mux.HandleFunc("/domain/", api.handleDomainLinks)

	// Network graph data
	mux.HandleFunc("/graph", api.handleGraph)

	// CORS middleware
	handler := corsMiddleware(mux)

	server := &http.Server{
		Addr:    ":" + api.port,
		Handler: handler,
	}

	color.New(color.FgGreen).Printf("🌐 API Server listening on http://localhost:%s\n", api.port)
	color.New(color.FgCyan).Println("Available endpoints:")
	color.New(color.FgCyan).Printf("  - GET http://localhost:%s/stats\n", api.port)
	color.New(color.FgCyan).Printf("  - GET http://localhost:%s/domains\n", api.port)
	color.New(color.FgCyan).Printf("  - GET http://localhost:%s/domain/{domain}/links?limit=50\n", api.port)
	color.New(color.FgCyan).Printf("  - GET http://localhost:%s/graph?min_links=1\n", api.port)

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Failed to start API server: %v", err)
	}
}

// handleStats returns overall statistics
func (api *APIService) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	totalURLs, err := GetTotalURLs(api.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	totalDomains, err := GetTotalDomains(api.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Get total links
	var totalLinks int
	err = api.db.QueryRow(`SELECT COUNT(*) FROM domain_links`).Scan(&totalLinks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Get pending URLs
	var pendingURLs int
	err = api.db.QueryRow(`SELECT COUNT(*) FROM urls WHERE status = 'pending'`).Scan(&pendingURLs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stats := map[string]interface{}{
		"total_urls_scraped": totalURLs,
		"total_domains":      totalDomains,
		"total_domain_links": totalLinks,
		"pending_urls":       pendingURLs,
		"timestamp":          time.Now().Format(time.RFC3339),
	}

	json.NewEncoder(w).Encode(stats)
}

// handleDomains returns statistics for all domains
func (api *APIService) handleDomains(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	domains, err := GetDomainStats(api.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"domains":   domains,
		"count":     len(domains),
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleDomainLinks returns the top links for a specific domain
func (api *APIService) handleDomainLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract domain from path: /domain/{domain}/links
	path := r.URL.Path
	domain := ""
	if len(path) > 8 { // "/domain/"
		domain = path[8:] // Extract everything after "/domain/"
		// Remove "/links" suffix if present
		if len(domain) > 6 && domain[len(domain)-6:] == "/links" {
			domain = domain[:len(domain)-6]
		}
	}

	if domain == "" {
		http.Error(w, "Domain not specified", http.StatusBadRequest)
		return
	}

	// Get limit from query parameter (default 50)
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	links, err := GetTopLinksForDomain(api.db, domain, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"domain":    domain,
		"links":     links,
		"count":     len(links),
		"limit":     limit,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleGraph returns network graph data for visualization
func (api *APIService) handleGraph(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Get min_links from query parameter (default 1)
	minLinksStr := r.URL.Query().Get("min_links")
	minLinks := 1
	if minLinksStr != "" {
		if m, err := strconv.Atoi(minLinksStr); err == nil && m >= 0 {
			minLinks = m
		}
	}

	graphData, err := GetGraphData(api.db, minLinks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"graph":     graphData,
		"min_links": minLinks,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// startClient starts the periodic POST client
func (api *APIService) startClient() {
	// Send immediately on startup
	api.sendDataToEndpoint()

	// Then send periodically
	ticker := time.NewTicker(api.frequency)
	defer ticker.Stop()

	for range ticker.C {
		api.sendDataToEndpoint()
	}
}

// sendDataToEndpoint sends all data to the configured destination
func (api *APIService) sendDataToEndpoint() {
	startTime := time.Now()
	color.New(color.FgYellow).Println("📤 API Client: Sending data to destination...")

	// Gather all data
	totalURLs, _ := GetTotalURLs(api.db)
	totalDomains, _ := GetTotalDomains(api.db)
	domains, _ := GetDomainStats(api.db)
	graphData, _ := GetGraphData(api.db, 1)

	// Create payload
	payload := map[string]interface{}{
		"total_urls_scraped": totalURLs,
		"total_domains":      totalDomains,
		"domains":            domains,
		"graph":              graphData,
		"timestamp":          time.Now().Format(time.RFC3339),
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("API Client error marshaling data: %v", err)
		return
	}

	// Send POST request
	resp, err := http.Post(api.destination, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("API Client error sending data: %v", err)
		return
	}
	defer resp.Body.Close()

	duration := time.Since(startTime)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		color.New(color.FgGreen).Printf("✅ API Client: Successfully sent data (status %d) in %v\n", resp.StatusCode, duration)
	} else {
		color.New(color.FgRed).Printf("❌ API Client: Failed to send data (status %d) in %v\n", resp.StatusCode, duration)
	}
}

// corsMiddleware adds CORS headers to allow cross-origin requests
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
