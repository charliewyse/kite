FROM golang:1.22-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o kite .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /app/kite /kite

EXPOSE 8080 9090
ENTRYPOINT ["/kite"]
