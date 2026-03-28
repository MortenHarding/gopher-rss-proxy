FROM golang:alpine

EXPOSE 7070/tcp

RUN apk --update add git \
&& git clone https://github.com/MortenHarding/gopher-rss-proxy.git \
&& cd gopher-rss-proxy \
&& go build -o /go/rssproxy . \
&& cp feeds.json /go/feeds.json \
&& cd /go \
&& rm -rf ./gopher-rss-proxy

WORKDIR /go

ENTRYPOINT ["./rssproxy"]
