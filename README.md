# RSS → Gopher Proxy

A lightweight proxy server written in Go that bridges the modern RSS/Atom web
and the classic Gopher protocol (RFC 1436).

## How it works

```
Gopher client  ──selector──►  rss-gopher-proxy  ──HTTP GET──►  RSS feed
               ◄──menu/text──                   ◄──XML─────────
```

| Selector pattern          | Response                                    |
|---------------------------|---------------------------------------------|
| *(empty)*                 | Welcome menu with usage and example feeds   |
| `/1/<feed-url>`           | Gopher menu — one text entry per RSS item   |
| `/0/<feed-url>/<index>`   | Plain-text rendering of a single RSS item   |

## Build

```bash
go build -o rss-gopher-proxy .
```

Or run directly:

```bash
go run main.go
```

## Usage

```
./rss-gopher-proxy [flags]

  -host string   Hostname/IP advertised in Gopher menus (default "localhost")
  -port int      TCP port to listen on (default 7070)
```

### Examples

```bash
# Start on the default port
./rss-gopher-proxy

# Start on port 70 (standard Gopher port — needs root or CAP_NET_BIND_SERVICE)
./rss-gopher-proxy -port 70 -host gopher.example.com
```

## Connecting

Use any Gopher client, e.g.:

```bash
# lynx
lynx gopher://localhost:7070

# curl (via gopher scheme)
curl gopher://localhost:7070/

# View a specific feed directly
curl "gopher://localhost:7070/1/https://news.ycombinator.com/rss"
```

### Selector format used internally

```
Welcome / root     →   (empty selector)
Feed menu          →   1/<https://...feed.rss>
Article text       →   0/<https://...feed.rss>/<item-index>
```

## Gopher item types used

| Type | Meaning      | Usage                         |
|------|--------------|-------------------------------|
| `i`  | Info         | Titles, descriptions, dates   |
| `0`  | Text file    | Individual RSS articles       |
| `1`  | Directory    | Feed listing / menus          |
| `3`  | Error        | Fetch or parse failures       |

## Features

- Pure stdlib Go — no external dependencies
- Parses RSS 2.0 feeds (including `content:encoded`)
- Strips HTML tags and decodes entities for clean terminal output
- Word-wraps article body at 72 columns
- Tries multiple date formats common in the wild
- 15-second HTTP timeout; caps feed bodies at 10 MB
- Handles multiple concurrent Gopher clients via goroutines

## Limitations

- RSS 2.0 only (Atom feeds not yet supported)
- No caching — every Gopher request fetches the feed live
- HTTPS feeds only via Go's default TLS stack (no custom CA support)

# view a specific feed directly
lynx "gopher://localhost:7070/1/https://news.ycombinator.com/rss"

# or with curl
curl "gopher://localhost:7070/1/https://lobste.rs/rss"