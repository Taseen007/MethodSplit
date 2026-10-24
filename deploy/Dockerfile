# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/${SERVICE}

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/service /app/service
ENTRYPOINT ["/app/service"]
