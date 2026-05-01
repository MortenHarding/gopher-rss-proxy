package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// ─── RSS Structures ───────────────────────────────────────────────────────────

type RSS struct {
	XMLName xml.Name `xml:"rss"`
	Channel Channel  `xml:"channel"`
}

type Channel struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Link        string `xml:"link"`
	Items       []Item `xml:"item"`
}

type Item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Content     string `xml:"encoded"` // content:encoded
	PubDate     string `xml:"pubDate"`
	Author      string `xml:"author"`
	GUID        string `xml:"guid"`
}

// ─── Gopher Protocol Constants ────────────────────────────────────────────────

const (
	TypeText      = '0' // plain text file
	TypeDirectory = '1' // directory / menu
	TypeInfo      = 'i' // informational message (no selector)
	TypeError     = '3' // error
	TypeURL       = 'h' // URL
)

// ─── Server ───────────────────────────────────────────────────────────────────

type GopherServer struct {
	host       string
	port       int
	feedConfig *FeedConfig
	accessLog  *AccessLogger
}

// ─── Feed Configuration ───────────────────────────────────────────────────────

type FeedEntry struct {
	Label    string `json:"label"`
	URL      string `json:"url"`      // RSS feed URL (mutually exclusive with Selector)
	Selector string `json:"selector"` // Raw gopher:// URL, e.g. "gopher://codevoid.de:70/1/cnn"
}

type FeedSection struct {
	Title string      `json:"title"`
	Feeds []FeedEntry `json:"feeds"`
}

type FeedConfig struct {
	Sections []FeedSection `json:"sections"`
}

// ─── Access Logger ────────────────────────────────────────────────────────────

type AccessLogger struct {
	logger *log.Logger
}

type LogEntry struct {
	Timestamp time.Time
	IP        string
	ItemType  string // "welcome", "dir", "text", "error"
	Selector  string
	ElapsedMS int64
	Status    string // "ok" or "error: <reason>"
}

func NewAccessLogger(path string) (*AccessLogger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening access log %q: %w", path, err)
	}
	return &AccessLogger{
		logger: log.New(f, "", 0),
	}, nil
}

func (l *AccessLogger) Log(e LogEntry) {
	if l == nil {
		return
	}
	l.logger.Printf("%s\t%s\t%-7s\t%s\t%dms\t%s",
		e.Timestamp.UTC().Format(time.RFC3339),
		e.IP,
		e.ItemType,
		e.Selector,
		e.ElapsedMS,
		e.Status,
	)
}

func loadFeedConfig(path string) (*FeedConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading feed config %q: %w", path, err)
	}
	var cfg FeedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing feed config %q: %w", path, err)
	}
	return &cfg, nil
}

func NewGopherServer(host string, port int, cfg *FeedConfig, al *AccessLogger) *GopherServer {
	return &GopherServer{host: host, port: port, feedConfig: cfg, accessLog: al}
}

func (s *GopherServer) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	log.Printf("Gopher RSS Proxy listening on gopher://%s", addr)
	log.Printf("Usage: gopher://%s/1/https://example.com/feed.rss", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *GopherServer) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		log.Printf("read error: %v", err)
		return
	}
	selector := strings.TrimRight(string(buf[:n]), "\r\n")
	log.Printf("request: %q", selector)

	start := time.Now()
	response, routeErr := s.route(selector)
	elapsed := time.Since(start).Milliseconds()

	if routeErr != nil {
		log.Printf("route error: %v", routeErr)
		response = gopherError(routeErr.Error())
	}
	conn.Write([]byte(response))

	// ── access log ──────────────────────────────────────────────
	ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	entry := LogEntry{
		Timestamp: time.Now().UTC(),
		IP:        ip,
		ItemType:  itemType(selector),
		Selector:  selector,
		ElapsedMS: elapsed,
		Status:    "ok",
	}
	if routeErr != nil {
		entry.Status = "error: " + routeErr.Error()
		entry.ItemType = "error"
	}
	s.accessLog.Log(entry)
	// ────────────────────────────────────────────────────────────
}

// route dispatches based on the selector path.
//
// Gopher clients (e.g. lynx) strip the leading "/<type>" from the selector
// before sending it to the server. So when the user types:
//
//	gopher://host:7070/1/https://example.com/rss
//
// lynx sends the selector:
//
//	https://example.com/rss          (bare feed URL, no type prefix)
//
// When clicking a menu entry whose selector is "/0/https://.../rss/3", lynx
// sends the selector WITH the leading slash but WITHOUT the type byte:
//
//	/https://example.com/rss/3
//
// We therefore need to handle several formats:
//
//	""                          -> welcome menu
//	"https://..."               -> feed menu  (bare URL, type stripped by client)
//	"/1/https://..."            -> feed menu  (full selector, type kept)
//	"/https://.../rss/3"        -> item text  (leading slash, type stripped)
//	"/0/https://.../rss/3"      -> item text  (full selector, type kept)
func (s *GopherServer) route(selector string) (string, error) {
	// Empty selector -> welcome
	if selector == "" || selector == "/" {
		return s.welcomeMenu(), nil
	}

	// --- Case 1: bare feed URL sent by lynx for command-line gopher:// URLs ---
	// lynx strips the item-type digit, so we receive "https://..." directly.
	if strings.HasPrefix(selector, "http://") || strings.HasPrefix(selector, "https://") {
		return s.feedMenu(selector)
	}

	// Ensure leading slash for all remaining cases.
	if !strings.HasPrefix(selector, "/") {
		selector = "/" + selector
	}

	// --- Case 2: full selector with type byte: "/X/<payload>" ---
	// selector[0]='/', selector[1]=type, selector[2]='/'
	if len(selector) >= 3 && selector[2] == '/' {
		gopherType := selector[1]
		payload := selector[3:] // everything after "/X/"

		switch gopherType {
		case '1':
			if payload == "" {
				return gopherError("no feed URL provided"), nil
			}
			return s.feedMenu(payload)

		case '0':
			return s.routeItemSelector(payload)

		default:
			// Fall through to Case 3 — maybe the type byte was stripped and
			// selector[1] is actually the start of the URL.
		}
	}

	// --- Case 3: leading slash but type byte stripped: "/https://<rest>" ---
	// After stripping the leading '/' we should have a feed URL or item path.
	stripped := strings.TrimPrefix(selector, "/")
	if strings.HasPrefix(stripped, "http://") || strings.HasPrefix(stripped, "https://") {
		// Could be a bare feed URL or an item selector "https://.../rss/3"
		// Distinguish by checking if the last segment is a plain integer.
		if feedURL, index, ok := splitItemSelector(stripped); ok {
			return s.itemText(feedURL, index)
		}
		return s.feedMenu(stripped)
	}

	return gopherError(fmt.Sprintf("unrecognised selector %q", selector)), nil
}

// routeItemSelector handles payload of the form "<feed-url>/<item-index>".
func (s *GopherServer) routeItemSelector(payload string) (string, error) {
	feedURL, index, ok := splitItemSelector(payload)
	if !ok {
		return gopherError("invalid item selector"), nil
	}
	return s.itemText(feedURL, index)
}

// splitItemSelector splits "https://example.com/rss/3" into
// ("https://example.com/rss", 3, true).  Returns (_, _, false) if the last
// path segment is not a non-negative integer.
func splitItemSelector(s string) (feedURL string, index int, ok bool) {
	idx := strings.LastIndex(s, "/")
	if idx < 0 {
		return "", 0, false
	}
	indexStr := s[idx+1:]
	var n int
	if _, err := fmt.Sscanf(indexStr, "%d", &n); err != nil {
		return "", 0, false
	}
	// Guard against a bare integer in the feed URL being mistaken for an index.
	feedURL = s[:idx]
	if !strings.HasPrefix(feedURL, "http://") && !strings.HasPrefix(feedURL, "https://") {
		return "", 0, false
	}
	return feedURL, n, true
}

// ─── Gopher URL Parsing ───────────────────────────────────────────────────────

// parseGopherURL parses a gopher:// URL into its host, port, item type, and
// selector components.
//
// In the Gopher URL scheme (RFC 4266) the path is "/<typebyte>/<selector>".
// The type byte is part of the URL path but must NOT be included in the
// selector field of a menu line — gopherLine() already encodes the type as
// the leading character of each line.
//
// Examples:
//
//	"gopher://codevoid.de:70/1/cnn"  -> "codevoid.de", 70, '1', "/cnn", nil
//	"gopher://codevoid.de/1/cnn"     -> "codevoid.de", 70, '1', "/cnn", nil
//	"gopher://codevoid.de:70/"       -> "codevoid.de", 70, '1', "",     nil
func parseGopherURL(raw string) (host string, port int, gopherType byte, selector string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("invalid gopher URL %q: %w", raw, err)
	}
	if u.Scheme != "gopher" {
		return "", 0, 0, "", fmt.Errorf("expected gopher:// scheme, got %q", u.Scheme)
	}
	host = u.Hostname()
	if host == "" {
		return "", 0, 0, "", fmt.Errorf("missing host in gopher URL %q", raw)
	}
	port = 70 // default Gopher port
	if ps := u.Port(); ps != "" {
		if _, scanErr := fmt.Sscanf(ps, "%d", &port); scanErr != nil {
			return "", 0, 0, "", fmt.Errorf("invalid port %q in gopher URL %q", ps, raw)
		}
	}
	// Path is "/<typebyte>/<selector>" per RFC 4266.
	// Strip the leading "/" and extract the type byte.
	gopherType = TypeDirectory // default to '1'
	path := strings.TrimPrefix(u.Path, "/")
	if len(path) >= 1 {
		gopherType = path[0]
		selector = path[1:] // everything after the type byte (may start with '/')
	}
	return host, port, gopherType, selector, nil
}

// ─── Welcome Menu ─────────────────────────────────────────────────────────────

func (s *GopherServer) welcomeMenu() string {
	var b strings.Builder
	writeInfo(&b, "RSS -> Gopher Proxy")
	writeInfo(&b, strings.Repeat("-", 32))

	if s.feedConfig == nil || len(s.feedConfig.Sections) == 0 {
		writeInfo(&b, "")
		writeInfo(&b, "(no feeds configured - see feeds.json)")
	} else {
		for _, section := range s.feedConfig.Sections {
			writeInfo(&b, "")
			writeInfo(&b, section.Title+":")
			for _, feed := range section.Feeds {
				if feed.Selector != "" {
					// External gopher selector — parse host/port/type/path from the URL.
					if h, p, t, sel, err := parseGopherURL(feed.Selector); err == nil {
						b.WriteString(gopherLine(t, feed.Label, sel, h, p))
					} else {
						writeInfo(&b, fmt.Sprintf("(bad selector for %q: %v)", feed.Label, err))
					}
				} else {
					selector := fmt.Sprintf("/1/%s", feed.URL)
					writeDir(&b, feed.Label, selector, s.host, s.port)
				}
			}
		}
	}

	writeInfo(&b, "")
	writeInfo(&b, "To view an RSS feed, connect to:")
	writeInfo(&b, fmt.Sprintf("  gopher://%s:%d/1/<feed-url>", s.host, s.port))
	b.WriteString(".\r\n")
	return b.String()
}

// ─── Feed Menu ────────────────────────────────────────────────────────────────

func (s *GopherServer) feedMenu(feedURL string) (string, error) {
	feed, err := fetchRSS(feedURL)
	if err != nil {
		return "", fmt.Errorf("could not fetch RSS: %w", err)
	}

	var b strings.Builder
	writeInfo(&b, feed.Channel.Title)
	writeInfo(&b, strings.Repeat("-", 32))
	if feed.Channel.Description != "" {
		writeInfo(&b, stripTags(feed.Channel.Description))
	}
	writeInfo(&b, "")

	for i, item := range feed.Channel.Items {

		// Show pub date as info line if available
		if item.PubDate != "" {
			if t, err := parseDate(item.PubDate); err == nil {
				writeInfo(&b, strings.Repeat("-", 3)+"  "+t.Format("Mon, 02 Jan 2006 15:04")+"  "+strings.Repeat("-", 3))
			}
		}

		title := cleanText(item.Title)
		if title == "" {
			title = fmt.Sprintf("Item %d", i+1)
		}
		// Truncate very long titles
		if len(title) > 70 {
			title = title[:67] + "..."
		}
		selector := fmt.Sprintf("/0/%s/%d", feedURL, i)
		writeText(&b, title, selector, s.host, s.port)

		// Show link to main article
		if item.Link != "" {
			writeURL(&b, "Article ->", item.Link, s.host, s.port)
		}
	}

	if len(feed.Channel.Items) == 0 {
		writeInfo(&b, "(no items found in feed)")
	}

	b.WriteString(".\r\n")
	return b.String(), nil
}

// ─── Item Text ────────────────────────────────────────────────────────────────

func (s *GopherServer) itemText(feedURL string, index int) (string, error) {
	feed, err := fetchRSS(feedURL)
	if err != nil {
		return "", fmt.Errorf("could not fetch RSS: %w", err)
	}
	if index < 0 || index >= len(feed.Channel.Items) {
		return "", fmt.Errorf("item index %d out of range (feed has %d items)", index, len(feed.Channel.Items))
	}

	item := feed.Channel.Items[index]

	// Build the text body using plain \n throughout.
	// We normalise to \r\n in one pass at the very end.
	var b strings.Builder

	// Header
	title := cleanText(item.Title)
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("-", min(len(title), 67)) + "\n")
	b.WriteString("\n")

	// Try to fetch the full article from the link first.
	// Fall back to the RSS description/content field if that fails.
	var body string
	var sourceNote string

	if item.Link != "" {
		articleText, fetchErr := fetchArticle(item.Link)
		if fetchErr == nil && len(strings.TrimSpace(articleText)) > 100 {
			body = articleText
		} else {
			if fetchErr != nil {
				log.Printf("article fetch failed for %s: %v", item.Link, fetchErr)
			}
			sourceNote = "[Full article unavailable - showing RSS summary]\n\n"
			body = item.Content
			if body == "" {
				body = item.Description
			}
		}
	} else {
		body = item.Content
		if body == "" {
			body = item.Description
		}
	}

	// htmlToText and wordWrap both use plain \n internally.
	body = htmlToText(body)
	body = wordWrap(body, 67)

	if sourceNote != "" {
		b.WriteString(sourceNote)
	}
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString(strings.Repeat("-", 67) + "\n")
	b.WriteString("[ End of article - " + title + " ]\n")

	// Single normalisation pass: replace every \n (that isn't already \r\n)
	// with \r\n, then terminate the text file with ".\r\n".
	return normaliseCRLF(b.String()) + ".\r\n", nil
}

// ─── RSS Fetching ─────────────────────────────────────────────────────────────

func fetchRSS(rawURL string) (*RSS, error) {
	// Basic sanity check
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https feeds are supported")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GopherRSSProxy/1.0")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}

	// Cap body at 10 MB to avoid memory issues
	limited := io.LimitReader(resp.Body, 10*1024*1024)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	var feed RSS
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("parsing RSS XML: %w", err)
	}
	return &feed, nil
}

// ─── Article Fetching ─────────────────────────────────────────────────────────

// fetchArticle retrieves a web page and returns its main textual content.
func fetchArticle(articleURL string) (string, error) {
	u, err := url.ParseRequestURI(articleURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http/https supported")
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequest("GET", articleURL, nil)
	if err != nil {
		return "", err
	}
	// Identify as a real browser so sites don't block us with a 403.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GopherRSSProxy/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Only process HTML responses.
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "text") {
		return "", fmt.Errorf("unsupported content-type: %s", ct)
	}

	limited := io.LimitReader(resp.Body, 5*1024*1024) // 5 MB cap
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("reading body: %w", err)
	}

	return extractMainContent(string(data)), nil
}

// extractMainContent pulls readable text out of an HTML page using text-density
// scoring. Rather than trying to guess noise by class names (which is fragile),
// we:
//  1. Strip element types that are structurally never article body
//  2. Try semantic tags in priority order (<article>, <main>)
//     but only accept them if they score well on text density
//  3. Fall back to scoring every <div>/<section> block and picking the winner
//  4. Last resort: <body> or full document
func extractMainContent(htmlStr string) string {
	// Stage 1: remove entire element types that never contain article body.
	htmlStr = reScript.ReplaceAllString(htmlStr, "")
	htmlStr = reStyle.ReplaceAllString(htmlStr, "")
	htmlStr = reComments.ReplaceAllString(htmlStr, "")
	htmlStr = reNav.ReplaceAllString(htmlStr, "")
	htmlStr = reHeader.ReplaceAllString(htmlStr, "")
	htmlStr = reFooter.ReplaceAllString(htmlStr, "")
	htmlStr = reAside.ReplaceAllString(htmlStr, "")

	lower := strings.ToLower(htmlStr)

	// Stage 2: try semantic tags — trust them only if they look like prose.
	for _, tag := range []string{"article", "main"} {
		if text := extractTag(htmlStr, lower, tag); text != "" {
			if wordCount(text) > 80 && textDensity(text) > 0.35 {
				return text
			}
		}
	}

	// Stage 3: score all <div> and <section> blocks by text density.
	if best := scoredBlocks(htmlStr, lower); best != "" {
		return best
	}

	// Stage 4: fall back to <body>, then whole document.
	if text := extractTag(htmlStr, lower, "body"); text != "" {
		return text
	}
	return htmlToText(htmlStr)
}

func extractTag(htmlStr, lower, tag string) string {
	open := "<" + tag
	close := "</" + tag + ">"
	start := strings.Index(lower, open)
	end := strings.LastIndex(lower, close)
	if start < 0 || end <= start {
		return ""
	}
	tagEnd := strings.Index(lower[start:], ">")
	if tagEnd < 0 {
		return ""
	}
	inner := htmlStr[start+tagEnd+1 : end]
	text := htmlToText(inner)
	if len(strings.TrimSpace(text)) < 80 {
		return ""
	}
	return text
}

// scoredBlocks finds the <div> or <section> with the highest text density.
func scoredBlocks(htmlStr, lower string) string {
	// Collect all top-level-ish block start positions.
	type block struct {
		start, end int
	}
	var blocks []block

	tags := []string{"div", "section", "p"}
	for _, tag := range tags {
		open := "<" + tag
		close := "</" + tag + ">"
		pos := 0
		for {
			s := strings.Index(lower[pos:], open)
			if s < 0 {
				break
			}
			s += pos
			// Find end of opening tag
			tagEnd := strings.Index(lower[s:], ">")
			if tagEnd < 0 {
				break
			}
			// Find matching close — search forward from after open tag
			e := strings.Index(lower[s+tagEnd:], close)
			if e < 0 {
				pos = s + 1
				continue
			}
			e = s + tagEnd + e + len(close)
			if e-s > 200 { // ignore tiny blocks
				blocks = append(blocks, block{s + tagEnd + 1, e - len(close)})
			}
			pos = s + 1
		}
	}

	bestScore := 0.0
	bestText := ""
	for _, bl := range blocks {
		if bl.end <= bl.start {
			continue
		}
		inner := htmlStr[bl.start:bl.end]
		rawLen := len(inner)
		if rawLen < 200 {
			continue
		}
		text := htmlToText(inner)
		wc := wordCount(text)
		if wc < 60 {
			continue
		}
		density := textDensity(text)
		// Score = word count * density — rewards long, prose-heavy blocks.
		score := float64(wc) * density
		if score > bestScore {
			bestScore = score
			bestText = text
		}
	}
	return bestText
}

// textDensity returns the ratio of non-space characters to total characters.
// Higher means more prose, lower means tag-heavy or sparse content.
func textDensity(text string) float64 {
	if len(text) == 0 {
		return 0
	}
	nonSpace := 0
	for _, r := range text {
		if r != ' ' && r != '\n' && r != '\t' && r != '\r' {
			nonSpace++
		}
	}
	return float64(nonSpace) / float64(len(text))
}

// wordCount counts whitespace-separated words in a string.
func wordCount(text string) int {
	return len(strings.Fields(text))
}

var (
	reScript   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reNav      = regexp.MustCompile(`(?is)<nav[^>]*>.*?</nav>`)
	reHeader   = regexp.MustCompile(`(?is)<header[^>]*>.*?</header>`)
	reFooter   = regexp.MustCompile(`(?is)<footer[^>]*>.*?</footer>`)
	reAside    = regexp.MustCompile(`(?is)<aside[^>]*>.*?</aside>`)
	reComments = regexp.MustCompile(`(?is)<!--.*?-->`)
	reTag      = regexp.MustCompile(`<[^>]+>`)
	reEntities = regexp.MustCompile(`&[a-zA-Z0-9#]+;`)
	reSpaces   = regexp.MustCompile(`[ \t]+`)
	reNewlines = regexp.MustCompile(`\n{3,}`)
)

// gopherLine constructs a single Gopher menu line:
//
//	<type><display>\t<selector>\t<host>\t<port>\r\n
func gopherLine(gopherType byte, display, selector, host string, port int) string {
	return fmt.Sprintf("%c%s\t%s\t%s\t%d\r\n", gopherType, display, selector, host, port)
}

func writeInfo(b *strings.Builder, text string) {
	b.WriteString(fmt.Sprintf("%c%s\tfake\t(NULL)\t70\r\n", TypeInfo, text))
}

func writeDir(b *strings.Builder, display, selector, host string, port int) {
	b.WriteString(gopherLine(TypeDirectory, display, selector, host, port))
}

func writeText(b *strings.Builder, display, selector, host string, port int) {
	b.WriteString(gopherLine(TypeText, display, selector, host, port))
}

func writeURL(b *strings.Builder, display, selector, host string, port int) {
	b.WriteString(gopherLine(TypeURL, display, "URL:"+selector, host, port))
}

func gopherError(msg string) string {
	return fmt.Sprintf("%c%s\t\t\t\r\n.\r\n", TypeError, msg)
}

// ─── Text Processing ──────────────────────────────────────────────────────────

// htmlToText converts basic HTML to plain readable text.
func htmlToText(h string) string {
	// Replace block-level tags with newlines
	blockTags := []struct{ tag, replacement string }{
		{"<br>", "\n"}, {"<br/>", "\n"}, {"<br />", "\n"},
		{"</p>", "\n\n"}, {"</div>", "\n"}, {"</li>", "\n"},
		{"</h1>", "\n\n"}, {"</h2>", "\n\n"}, {"</h3>", "\n\n"},
		{"</h4>", "\n\n"}, {"</h5>", "\n\n"}, {"</h6>", "\n\n"},
		{"<li>", "* "}, {"<ul>", "\n"}, {"</ul>", "\n"},
		{"<ol>", "\n"}, {"</ol>", "\n"},
		{"<hr>", "\n" + strings.Repeat("-", 67) + "\n"},
		{"<hr/>", "\n" + strings.Repeat("-", 67) + "\n"},
	}
	lower := strings.ToLower(h)
	result := h
	for _, bt := range blockTags {
		result = strings.ReplaceAll(result, bt.tag, bt.replacement)
		result = strings.ReplaceAll(result, strings.ToUpper(bt.tag), bt.replacement)
		_ = lower // used above to check
	}

	// Strip remaining tags
	result = reTag.ReplaceAllString(result, "")

	// Decode HTML entities
	result = html.UnescapeString(result)

	// Normalise whitespace
	lines := strings.Split(result, "\n")
	var cleaned []string
	for _, line := range lines {
		line = reSpaces.ReplaceAllString(line, " ")
		line = strings.TrimRight(line, " \t")
		cleaned = append(cleaned, line)
	}
	result = strings.Join(cleaned, "\n")
	result = reNewlines.ReplaceAllString(result, "\n\n")
	result = strings.TrimSpace(result)
	return result
}

// stripTags removes HTML tags and returns plain text (single line).
func stripTags(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// cleanText removes HTML and normalises whitespace from a single-line field.
func cleanText(s string) string {
	return stripTags(s)
}

// wordWrap wraps text at the given column width, preserving existing newlines.
// Always uses \n as the line ending — callers normalise to \r\n if needed.
func wordWrap(text string, width int) string {
	var out strings.Builder
	paragraphs := strings.Split(text, "\n")
	for i, para := range paragraphs {
		if i > 0 {
			out.WriteString("\n")
		}
		if para == "" {
			continue
		}
		words := strings.Fields(para)
		lineLen := 0
		for j, word := range words {
			if j == 0 {
				out.WriteString(word)
				lineLen = len(word)
				continue
			}
			if lineLen+1+len(word) > width {
				out.WriteString("\n")
				out.WriteString(word)
				lineLen = len(word)
			} else {
				out.WriteString(" ")
				out.WriteString(word)
				lineLen += 1 + len(word)
			}
		}
	}
	return out.String()
}

// normaliseCRLF converts all bare \n to \r\n, leaving existing \r\n intact.
func normaliseCRLF(s string) string {
	// First strip any existing \r to avoid doubling them, then add \r before every \n.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return s
}

// parseDate tries common RSS date formats.
func parseDate(s string) (time.Time, error) {
	formats := []string{
		time.RFC1123Z,
		time.RFC1123,
		time.RFC3339,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 MST",
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	s = strings.TrimSpace(s)
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date: %q", s)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// itemType infers a human-readable request type from the selector.
func itemType(selector string) string {
	if selector == "" || selector == "/" {
		return "welcome"
	}
	// Bare feed URL (type digit stripped by client)
	if strings.HasPrefix(selector, "http://") || strings.HasPrefix(selector, "https://") {
		if _, _, ok := splitItemSelector(selector); ok {
			return "text"
		}
		return "dir"
	}
	// Full selector: "/X/<payload>"
	if len(selector) >= 3 && selector[0] == '/' && selector[2] == '/' {
		switch selector[1] {
		case '0':
			return "text"
		case '1':
			return "dir"
		}
	}
	// Leading slash, type stripped: "/https://..."
	stripped := strings.TrimPrefix(selector, "/")
	if strings.HasPrefix(stripped, "http://") || strings.HasPrefix(stripped, "https://") {
		if _, _, ok := splitItemSelector(stripped); ok {
			return "text"
		}
		return "dir"
	}
	return "unknown"
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	host := flag.String("host", "localhost", "hostname / IP to advertise in Gopher menus")
	port := flag.Int("port", 7070, "TCP port to listen on")
	feeds := flag.String("feeds", "feeds.json", "path to JSON feed config file")
	logFile := flag.String("access-log", "access.log", "path to access log file (empty to disable)")
	flag.Parse()

	cfg, err := loadFeedConfig(*feeds)
	if err != nil {
		log.Fatalf("feed config error: %v", err)
	}

	var al *AccessLogger
	if *logFile != "" {
		al, err = NewAccessLogger(*logFile)
		if err != nil {
			log.Fatalf("access log error: %v", err)
		}
		log.Printf("Access log: %s", *logFile)
	}

	srv := NewGopherServer(*host, *port, cfg, al)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
