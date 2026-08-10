FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/gemini-key-gateway .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/gemini-key-gateway /app/gemini-key-gateway
COPY config.example.json /app/config.example.json
EXPOSE 8080
ENTRYPOINT ["/app/gemini-key-gateway", "-config", "/app/config.json"]
