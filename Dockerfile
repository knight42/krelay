# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum* ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o krelay-server ./cmd/krelay-server

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/krelay-server /app/krelay-server

# Create non-root user
RUN adduser -D -u 1000 krelay
USER krelay

EXPOSE 8443

ENTRYPOINT ["/app/krelay-server"]
