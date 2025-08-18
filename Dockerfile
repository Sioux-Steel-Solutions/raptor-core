# Builder stage
FROM golang:1.21 AS builder

WORKDIR /app

# Copy go.mod, go.sum, and source
COPY go.mod go.sum ./
COPY main.go ./

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o raptor-core

# Runtime image
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/raptor-core .
CMD ["./raptor-core"]

