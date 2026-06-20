# ---- build stage ----
FROM golang:alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
RUN git clone https://github.com/MortenHarding/gopher-rss-proxy.git . \
    && go build -o /go/rssproxy .

# ---- final stage ----
FROM alpine
COPY --from=builder /go/rssproxy /rssproxy
COPY --from=builder /build/feeds.json /feeds.json
EXPOSE 7070/tcp
WORKDIR /
ENTRYPOINT ["./rssproxy"]