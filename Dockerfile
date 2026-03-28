FROM golang:alpine

EXPOSE 7070/tcp

RUN apk --update add git \
&& git clone https://github.com/MortenHarding/gopher-rss-proxy.git \
&& cd gopher-rss-proxy \
&& go build

COPY gopher-rss-proxy /bin/gopher-rss-proxy

#ENTRYPOINT ["/bin/gopher-rss-proxy"]
ENTRYPOINT [ "sh" ]