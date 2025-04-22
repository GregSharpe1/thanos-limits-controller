# Stage 1: Build the Go application
FROM golang:1.24-alpine AS builder

# Set working directory
WORKDIR /app

# Install necessary build tools
RUN apk add --no-cache git

# Copy go.mod and go.sum files first for better caching
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

# Build the application with optimizations:
# - CGO_ENABLED=0: pure Go implementation (no C dependencies)
# - -ldflags="-s -w": strip debug information and symbol tables
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/thanos-limits-controller .

# Stage 2: Create the minimal runtime image
FROM alpine:3.19

# Install CA certificates for HTTPS connections
RUN apk --no-cache add ca-certificates tzdata

# Create a non-root user to run the application
RUN adduser -D -H -h /app appuser
WORKDIR /app

# Copy only the built binary from the builder stage
COPY --from=builder /app/thanos-limits-controller .

# Set ownership
RUN chown -R appuser:appuser /app

# Use the non-root user
USER appuser

# Command to run the application
CMD ["./thanos-limits-controller"]
