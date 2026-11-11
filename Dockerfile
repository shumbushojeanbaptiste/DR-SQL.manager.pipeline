ARG POSTGRES_CLIENT_VERSION=18

FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY scripts ./scripts
RUN CGO_ENABLED=0 go build -o /out/dr-api ./cmd/api \
    && CGO_ENABLED=0 go build -o /out/dr-worker ./cmd/worker \
    && CGO_ENABLED=0 go build -o /out/dr-migrate ./scripts

FROM postgres:${POSTGRES_CLIENT_VERSION}
WORKDIR /app
COPY --from=builder /out/dr-api /app/dr-api
COPY --from=builder /out/dr-worker /app/dr-worker
COPY --from=builder /out/dr-migrate /app/dr-migrate
COPY migrations /app/migrations
ENTRYPOINT ["/app/dr-worker"]
