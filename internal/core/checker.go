package core

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Checker struct {
	Client       *http.Client
	UserAgent    string
	GlobalSem    chan struct{}
	PerHostSem   sync.Map // host -> chan struct{}
	HostDelay    time.Duration // base delay between requests to same host (jittered)
	MaxPerHost   int
}

func NewChecker(ua string) *Checker {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Checker{
		Client:     &http.Client{ Transport: tr, Timeout: 15 * time.Second },
		UserAgent:  ua,
		GlobalSem:  make(chan struct{}, 8),
		HostDelay:  600 * time.Millisecond,
		MaxPerHost: 2,
	}
}

func (c *Checker) hostSem(host string) chan struct{} {
	v, ok := c.PerHostSem.Load(host)
	if ok { return v.(chan struct{}) }
	ch := make(chan struct{}, c.MaxPerHost)
	actual, _ := c.PerHostSem.LoadOrStore(host, ch)
	return actual.(chan struct{})
}

func (c *Checker) jitterDelay() time.Duration { return c.HostDelay + time.Duration(rand.Intn(100)-50)*time.Millisecond }

func (c *Checker) CheckOnce(ctx context.Context, u *url.URL) (int, string, time.Duration, error) {
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.Client.Do(req)
	if err != nil || resp.StatusCode >= 400 || resp.StatusCode == 405 {
		if resp != nil { resp.Body.Close() }
		req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		req2.Header.Set("User-Agent", c.UserAgent)
		resp, err = c.Client.Do(req2)
		if err != nil { return 0, "", time.Since(start), err }
	}
	defer resp.Body.Close()
	final := resp.Request.URL.String()
	return resp.StatusCode, final, time.Since(start), nil
}

func (c *Checker) CheckURL(ctx context.Context, u *url.URL) (int, string, time.Duration, error) {
	c.GlobalSem <- struct{}{}; defer func(){ <-c.GlobalSem }()
	hsem := c.hostSem(u.Hostname())
	hsem <- struct{}{}; defer func(){ <-hsem }()

	select { case <- time.After(c.jitterDelay()): case <- ctx.Done(): return 0, "", 0, ctx.Err() }
	code, final, d, err := c.CheckOnce(ctx, u)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || strings.Contains(err.Error(), "timeout") {
			select { case <- time.After(500 * time.Millisecond): case <- ctx.Done(): return 0, "", d, ctx.Err() }
			code2, final2, d2, err2 := c.CheckOnce(ctx, u)
			if err2 == nil { return code2, final2, d + d2, nil }
		}
	}
	return code, final, d, err
}
