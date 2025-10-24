
package core

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/temoto/robotstxt"
)

type RobotsGuard struct {
	allowed bool
	rules   *robotstxt.RobotsData
	agent   string
}

func FetchRobots(client *http.Client, start *url.URL, ua string) *RobotsGuard {
	robotURL := &url.URL{Scheme: start.Scheme, Host: start.Host, Path: "/robots.txt"}
	req, _ := http.NewRequest(http.MethodGet, robotURL.String(), nil)
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil { resp.Body.Close() }
		return &RobotsGuard{allowed: true, rules: nil, agent: ua} // fail-open
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return &RobotsGuard{allowed: true, rules: nil, agent: ua}
	}
	r, err := robotstxt.FromBytes(body)
	if err != nil {
		return &RobotsGuard{allowed: true, rules: nil, agent: ua}
	}
	return &RobotsGuard{allowed: true, rules: r, agent: ua}
}

func (rg *RobotsGuard) Allowed(u *url.URL) bool {
	if rg == nil || !rg.allowed { return true }
	if rg.rules == nil { return true }
	grp := rg.rules.FindGroup(rg.agent)
	if grp == nil { grp = rg.rules.FindGroup("*") }
	if grp == nil { return true }
	return grp.Test(u.Path)
}

func HumanizeCrawlDelay(rg *RobotsGuard) time.Duration {
	if rg == nil || rg.rules == nil { return 0 }
	grp := rg.rules.FindGroup(rg.agent)
	if grp == nil { grp = rg.rules.FindGroup("*") }
	if grp == nil { return 0 }
	return grp.CrawlDelay
}

func RobotsInfoString(rg *RobotsGuard) string {
	if rg == nil { return "robots: n/a" }
	parts := []string{"robots: on"}
	cd := HumanizeCrawlDelay(rg)
	if cd > 0 { parts = append(parts, fmt.Sprintf("crawl-delay=%s", cd)) }
	return strings.Join(parts, ", ")
}
