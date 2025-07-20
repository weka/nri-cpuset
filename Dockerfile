# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o weka-cpuset ./cmd/weka-cpuset

# Runtime stage
FROM alpine:3.20

RUN apk --no-cache add ca-certificates
WORKDIR /

COPY --from=builder /src/weka-cpuset /usr/local/bin/weka-cpuset

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/weka-cpuset"]