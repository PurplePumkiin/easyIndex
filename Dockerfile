# Build stage
FROM golang:1.26-alpine AS builder

# Install build dependencies
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY *.go ./

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o goindexer .

# Runtime stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates sqlite-libs

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/goindexer .

# Create directories for volumes
RUN mkdir -p /app/data /app/db

# Declare volumes (can be mapped by users)
VOLUME ["/app/data", "/app/db"]

# Run as non-root user for security
RUN adduser -D -u 1000 goindexer && \
    chown -R goindexer:goindexer /app
USER goindexer

# Run the application
CMD ["./goindexer"]
