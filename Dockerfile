# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Install git and build essentials
RUN apk add --no-cache git ca-certificates

# Cache dependencies
COPY go.mod ./
RUN go mod download || true

COPY . .

# Fetch exact dependencies if go.sum not present
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ironbot main.go

# Runtime stage (minimal and isolated)
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

# Create isolated non-root user
RUN addgroup -S botgroup && adduser -S botuser -G botgroup

WORKDIR /home/botuser
COPY --from=builder /app/ironbot /usr/local/bin/ironbot

USER botuser

# Default environment variables
ENV HOST=ayx.acido.dev \
    PORT=19132 \
    BOT_NAME=IronBot \
    TRANSPORT=nethernet \
    GAME_VERSION=1.26.61 \
    AUTH_INPUT=false

ENTRYPOINT ["sh", "-c", "exec /usr/local/bin/ironbot -host \"$HOST\" -port \"$PORT\" -name \"$BOT_NAME\" -transport \"$TRANSPORT\" -game-version \"$GAME_VERSION\" -auth-input=$AUTH_INPUT"]
