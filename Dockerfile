FROM golang:alpine

EXPOSE 7070/tcp

RUN apk --update add git \
&& git clone https://github.com/MortenHarding/gopher-rss-proxy.git \
&& cd gopher-rss-proxy \
&& go build -o /go/rssproxy . \
&& cd /go \
&& rm -rf ./gopher-rss-proxy

WORKDIR /go

COPY feeds.json /go/feeds.json

ENTRYPOINT ["./rssproxy"]
