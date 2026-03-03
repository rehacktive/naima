FROM golang:1.25-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/naima .


FROM debian:bookworm-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates tzdata \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/naima /app/naima
COPY prompt.txt /app/prompt.txt
COPY internal/httpapi/ui /app/internal/httpapi/ui

EXPOSE 8080

ENTRYPOINT ["/app/naima"]
