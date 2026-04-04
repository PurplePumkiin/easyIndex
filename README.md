# GOindexer

A high-performance, multi-threaded web crawler written in Go with SQLite storage and Docker support.

## Features

- 🚀 **Multi-threaded**: Concurrent workers for efficient crawling
- 🤖 **Robots.txt compliant**: Respects crawl delays and disallow rules
- 💾 **SQLite storage**: Lightweight database for URL tracking
- 🐳 **Docker ready**: Easy deployment with volume support
- 📦 **Self-contained**: No external dependencies required
- 🔄 **Auto-discovery**: Automatically discovers new domains while crawling

## Quick Start

### Using Docker (Recommended)

1. **Clone the repository**
   ```bash
   git clone https://github.com/PurplePumkiin/easyIndex.git
   cd easyIndex
   ```

2. **Create your .env file**
   ```bash
   cp .env.example .env
   # Edit .env with your preferred settings
   ```

3. **Run with Docker Compose**
   ```bash
   docker-compose up -d
   ```

4. **Check logs**
   ```bash
   docker-compose logs -f
   ```

### Running Locally

1. **Prerequisites**
   - Go 1.20 or higher
   - GCC (for SQLite compilation)

2. **Install dependencies**
   ```bash
   go mod download
   ```

3. **Configure environment**
   ```bash
   cp .env.example .env
   # Edit .env with your settings
   ```

4. **Run**
   ```bash
   go run main.go
   ```

## Configuration

Edit `.env` file:

```env
# Starting URL for the crawler
STARTING_URL=https://en.wikipedia.org/wiki/Main_Page

# Number of concurrent workers (default: 10)
NUM_WORKERS=10

# Default crawl delay in milliseconds (default: 2000)
DEFAULT_CRAWL_DELAY_MS=2000

# If you want to save the pages crawled, set this to true
SAVE_DATA=false
```

## Docker Volumes

The application uses two volumes for persistent data:

- `/app/data` - Downloaded HTML files (organized by domain)
- `/app/db` - SQLite database with crawl state

**Example with custom paths:**
```bash
docker run -v /my/data:/app/data \
           -v /my/database:/app/db \
           -v ./.env:/app/.env:ro \
           ghcr.io/PurplePumkiin/easyIndex:latest
```

## Architecture

- **Worker Pool**: Concurrent goroutines processing domains
- **Domain Registry**: In-memory tracking of which worker owns which domain
- **SQLite WAL**: Write-Ahead Logging for concurrent write performance
- **Claim System**: 5-minute timeout for automatic recovery from worker crashes

## Database Schema

### URLs Table
- Tracks all discovered URLs
- Status: pending, fetched, failed, skipped
- Includes source URL for backtracking

### Domains Table
- Stores robots.txt per domain
- Tracks crawl delays
- Worker claim system with timestamps

## Performance Considerations

**Database Growth:**
- Expect ~1GB per 1-2 hours of crawling on Wikipedia-scale sites
- Use external volumes for production deployments
- Consider periodic database optimization (`VACUUM`)

**Resource Usage:**
- Memory: ~500MB baseline + ~50MB per worker
- CPU: Scales linearly with worker count
- Network: Depends on crawl delay and target sites

## Development

### Building
```bash
go build -o goindexer main.go
```

### Testing
```bash
# Test with 1 worker
NUM_WORKERS=1 go run main.go
```

## Contributing

Contributions welcome! Please:
1. Fork the repository
2. Create a feature branch
3. Submit a pull request

## Disclaimer

**Respect robots.txt and website terms of service.** This tool is for educational and research purposes. Users are responsible for ensuring their usage complies with applicable laws and website policies.

## Roadmap

- [ ] Sitemap.xml parsing
- [ ] Content compression before storage
- [ ] S3 upload support
- [ ] Prometheus metrics
- [ ] Web dashboard for monitoring
- [ ] Domain filtering/allowlisting
- [ ] Custom user-agent configuration

## Support

For issues, questions, or contributions, please open an issue on GitHub.
