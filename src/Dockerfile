# Stage 1: build the binary.
#
# The builder runs natively on whatever machine you are on, which on an Apple
# Silicon Mac is arm64. GOARCH=amd64 cross-compiles the output for Fargate,
# which only runs amd64. Cross-compiling is far faster than emulating an
# amd64 builder, and Go makes it a one-word change.
FROM golang:1.27 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o product-service .

# Stage 2: ship only the binary.
#
# This stage is pinned to linux/amd64 so the final image is an amd64 image
# even when built on an arm64 Mac. Without this pin the image would be arm64
# and the Fargate task would fail to start with an exec format error.
FROM --platform=linux/amd64 alpine:latest

WORKDIR /root/

COPY --from=builder /app/product-service .

EXPOSE 8080

CMD ["./product-service"]
