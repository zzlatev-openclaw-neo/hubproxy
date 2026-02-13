FROM golang:1.24-alpine AS builder

# Install build dependencies for CGO/SQLite
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build with CGO enabled for SQLite
RUN CGO_ENABLED=1 go build -o /hubproxy ./cmd/hubproxy

FROM alpine:3.19

# Install runtime dependencies for SQLite
RUN apk add --no-cache sqlite-libs ca-certificates

WORKDIR /app

COPY --from=builder /hubproxy /hubproxy

EXPOSE 8080 8081
CMD ["/hubproxy"]
