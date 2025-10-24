package core

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

var stripParams = map[string]struct{}{
	"utm_source": {}, "utm_medium": {}, "utm_campaign": {}, "utm_term": {}, "utm_content": {},
	"gclid": {}, "fbclid": {}, "mc_cid": {}, "mc_eid": {},
}

var reDefaultPorts = regexp.MustCompile(`:(80|443)$`)

type DomainNormalizer struct {
	rootETLD1 string
}

func NewDomainNormalizer(start *url.URL) *DomainNormalizer {
	etld1, err := publicsuffix.EffectiveTLDPlusOne(start.Hostname())
	if err != nil { etld1 = start.Hostname() }
	return &DomainNormalizer{rootETLD1: strings.ToLower(etld1)}
}

func (d *DomainNormalizer) IsInternal(u *url.URL) bool {
	if u == nil { return false }
	etld1, err := publicsuffix.EffectiveTLDPlusOne(u.Hostname())
	if err != nil { return false }
	return strings.EqualFold(etld1, d.rootETLD1)
}

func (d *DomainNormalizer) Normalize(u *url.URL) string {
	if u == nil { return "" }
	uu := *u
	uu.Fragment = ""

	uu.Scheme = strings.ToLower(uu.Scheme)
	uu.Host = strings.ToLower(uu.Host)
	uu.Host = reDefaultPorts.ReplaceAllString(uu.Host, "")

	uu.Path = path.Clean(uu.Path)
	if uu.Path == "." { uu.Path = "/" }

	if uu.RawQuery != "" {
		vals := uu.Query()
		keys := make([]string, 0, len(vals))
		for k := range vals { keys = append(keys, k) }
		slices.Sort(keys)
		for _, k := range keys {
			if _, ok := stripParams[strings.ToLower(k)]; ok { vals.Del(k) }
		}
		uu.RawQuery = vals.Encode()
	}
	return uu.String()
}
