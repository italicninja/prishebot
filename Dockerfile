FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o prishe .

FROM alpine:3.19
# ca-certificates: needed for HTTPS (Tenor API, Discord)
# tzdata: needed for birthday midnight-UTC calculations
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/prishe .
EXPOSE 8080
CMD ["./prishe"]
