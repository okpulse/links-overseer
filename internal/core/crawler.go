package core

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Crawler struct {
	Client        *http.Client
	Checker       *Checker
	Normalizer    *DomainNormalizer
	RespectRobots bool
	Robots        *RobotsGuard

	MaxDepth int
	MaxURLs  int // hard cap per job

	seenPages sync.Map // normalized URL -> depth
	seenLinks sync.Map // normalized URL -> struct{}
	checkedLinks int
	totalLinks   int
}

func NewCrawler(start *url.URL, maxDepth int, respectRobots bool, checker *Checker) *Crawler {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{ Timeout: 10 * time.Second, KeepAlive: 30 * time.Second }).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{ Transport: tr, Timeout: 15 * time.Second }
	norm := NewDomainNormalizer(start)
	c := &Crawler{
		Client:        client,
		Checker:       checker,
		Normalizer:    norm,
		RespectRobots: respectRobots,
		MaxDepth:      maxDepth,
		MaxURLs:       2000,
	}
	if respectRobots { c.Robots = FetchRobots(client, start, checker.UserAgent); cd := HumanizeCrawlDelay(c.Robots); if cd > 0 && cd > checker.HostDelay { checker.HostDelay = cd } }
	return c
}

type CrawlProgress struct {
	Visited int
	Queued int
	Discovered int
	Errors int
	CheckedLinks int
	TotalLinks int
}

func (c *Crawler) shouldVisit(u *url.URL, depth int) bool {
	if depth > c.MaxDepth { return false }
	if u.Scheme != "http" && u.Scheme != "https" { return false }
	if !c.Normalizer.IsInternal(u) { return false }
	norm := c.Normalizer.Normalize(u)
	if _, seen := c.seenPages.Load(norm); seen { return false }
	if c.RespectRobots && c.Robots != nil && !c.Robots.Allowed(u) { return false }
	return true
}

func isPrivateHost(u *url.URL) bool {
	h := u.Hostname()
	if h == "localhost" { return true }
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() { return true }
	}
	return false
}

func (c *Crawler) fetch(ctx context.Context, u *url.URL) (*http.Response, error) {
	if isPrivateHost(u) { return nil, errors.New("blocked private/loopback host") }
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", c.Checker.UserAgent)
	t := 400 + rand.Intn(300)
	select { case <- time.After(time.Duration(t)*time.Millisecond): case <- ctx.Done(): return nil, ctx.Err() }
	resp, err := c.Client.Do(req)
	if err != nil { return nil, err }
	return resp, nil
}

func (c *Crawler) extractLinks(resp *http.Response, base *url.URL) []*url.URL {
	defer resp.Body.Close()
	n := []*url.URL{}
	doc, err := html.Parse(resp.Body)
	if err != nil { return n }
	var f func(*html.Node)
	f = func(node *html.Node) {
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "a") {
			for _, a := range node.Attr {
				if strings.EqualFold(a.Key, "href") {
					raw := strings.TrimSpace(a.Val)
					if raw == "" { break }
					u2, err := base.Parse(raw)
					if err == nil {
						if u2.Scheme == "http" || u2.Scheme == "https" {
							n = append(n, u2)
						}
					}
					break
				}
			}
		}
		for cnode := node.FirstChild; cnode != nil; cnode = cnode.NextSibling { f(cnode) }
	}
	f(doc)
	return n
}


func (c *Crawler) checkLinkAndSink(ctx context.Context, page *url.URL, u *url.URL, internal bool, sink func(Result), progress func(CrawlProgress)) {
	code, _, dur, err := c.Checker.CheckURL(ctx, u)
	res := Result{
		URL: u.String(),
		PageURL: page.String(),
		Internal: internal,
		StatusCode: code,
		ElapsedMS: dur.Milliseconds(),
		Depth: 0,
	}
	if err != nil { res.Err = err.Error() }
	sink(res)

	c.checkedLinks++
	if progress != nil { progress(CrawlProgress{Visited: 0, Queued: 0, Discovered: 0, Errors: 0, CheckedLinks: c.checkedLinks, TotalLinks: c.totalLinks}) }
}

func (c *Crawler) Crawl(ctx context.Context, start *url.URL, progress func(CrawlProgress), sink func(Result)) error {
	type Item struct { u *url.URL; depth int }
	q := make(chan Item, 1024)
	visited, discovered, errs := 0, 0, 0

	if !c.shouldVisit(start, 0) { return fmt.Errorf("start URL not allowed or already seen") }
	normStart := c.Normalizer.Normalize(start)
	c.seenPages.Store(normStart, 0)
	discovered++
	q <- Item{u: start, depth: 0}

	for {
		select {
		case <-ctx.Done(): return ctx.Err()
		case it := <-q:
			if it.u == nil { return nil }
			visited++
			if progress != nil { progress(CrawlProgress{Visited: visited, Queued: len(q), Discovered: discovered, Errors: errs, CheckedLinks: c.checkedLinks, TotalLinks: c.totalLinks}) }

			resp, err := c.fetch(ctx, it.u)
			if err != nil {
				sink(Result{URL: it.u.String(), PageURL: it.u.String(), Internal: true, StatusCode: 0, Err: err.Error(), ElapsedMS: 0, Depth: it.depth})
				errs++
				continue
			}
			base := resp.Request.URL
			links := c.extractLinks(resp, base)

			for _, u2 := range links {
				norm := c.Normalizer.Normalize(u2)
				if _, seen := c.seenLinks.LoadOrStore(norm, struct{}{}); !seen {
					c.totalLinks++
					if progress != nil { progress(CrawlProgress{Visited: visited, Queued: len(q), Discovered: discovered, Errors: errs, CheckedLinks: c.checkedLinks, TotalLinks: c.totalLinks}) }
					// stream check immediately
					go c.checkLinkAndSink(ctx, base, u2, c.Normalizer.IsInternal(u2), sink, progress)
				}
				if c.shouldVisit(u2, it.depth+1) {
					if discovered >= c.MaxURLs { continue }
					c.seenPages.Store(c.Normalizer.Normalize(u2), it.depth+1)
					discovered++
					q <- Item{u: u2, depth: it.depth+1}
				}
			}
		}
	}
}

