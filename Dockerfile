# Build stage
FROM golang:1.23 AS builder

WORKDIR /app
ENV GOPROXY=direct
COPY go.mod go.sum ./
RUN go mod tidy && go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o sns-subscriber main.go

# Final minimal image
FROM alpine:latest

WORKDIR /root/

COPY --from=builder /app/sns-subscriber .

RUN apk --no-cache add ca-certificates

CMD ["./sns-subscriber"]
