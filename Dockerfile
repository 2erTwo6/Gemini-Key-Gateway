FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/scorpio-balance .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/scorpio-balance /app/scorpio-balance
COPY config.example.json /app/config.example.json
EXPOSE 8080
ENTRYPOINT ["/app/scorpio-balance", "-config", "/app/config.json"]
