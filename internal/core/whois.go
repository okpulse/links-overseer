package core

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
)

// WhoisInfo 
type WhoisInfo struct {
	Domain         string   `json:"domain"`
	Registrar      string   `json:"registrar"`
	Registrant     string   `json:"registrant"`
	CreationDate   string   `json:"creation_date"`
	UpdatedDate    string   `json:"updated_date"`
	ExpirationDate string   `json:"expiration_date"`
	NameServers    []string `json:"name_servers"`
	Emails         []string `json:"emails"`
	Raw            string   `json:"raw"`
}

// FetchWhois 
// target может быть как "https://example.com", так и "example.com"
func FetchWhois(ctx context.Context, target string) (*WhoisInfo, error) {
	domain := normalizeDomain(target)
	if domain == "" {
		return nil, errors.New("empty domain")
	}

	raw, err := fetchWhoisRaw(ctx, domain)
	if err != nil {
		return nil, err
	}
	info := parseWhois(domain, raw)
	return &info, nil
}

// normalizeDomain приводит введённую строку к доменному имени
func normalizeDomain(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	// распарсить как URL.
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	// считаем, что это доменное имя
	target = strings.TrimPrefix(target, "www.")
	return strings.ToLower(target)
}

// обращается к whois.iana.org, чтобы получить конкретный WHOIS-сервер,
// если не удался, возвращает ответ IANA
func fetchWhoisRaw(ctx context.Context, domain string) (string, error) {
	rawIANA, err := whoisQuery(ctx, "whois.iana.org", domain)
	if err != nil {
		return "", err
	}
	server := parseWhoisServer(rawIANA)
	if server == "" {
		// Не нашли сервер
		return rawIANA, nil
	}

	raw, err := whoisQuery(ctx, server, domain)
	if err != nil {
		// не отвечает
		return rawIANA, nil
	}
	return raw, nil
}

// whoisQuery запрос к WHOIS-серверу по TCP:43
func whoisQuery(ctx context.Context, server, query string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(server, "43"))
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, query+"\r\n"); err != nil {
		return "", err
	}
	buf, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// parseWhoisServer извлекает строку whois-сервера вида "whois: whois.example.net"
func parseWhoisServer(raw string) string {
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "whois:") {
			v := strings.TrimSpace(line[len("whois:"):])
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// parseWhois вытаскивает ключевые поля
func parseWhois(domain, raw string) WhoisInfo {
	info := WhoisInfo{
		Domain: strings.ToLower(domain),
		Raw:    raw,
	}

	var nameServers []string
	emailSet := map[string]struct{}{}

	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "%") {
			continue
		}
		lower := strings.ToLower(line)

		// Базовые поля
		switch {
		case strings.HasPrefix(lower, "domain name:"):
			info.Domain = strings.TrimSpace(line[len("domain name:"):])

		case strings.HasPrefix(lower, "registrar:"):
			if info.Registrar == "" {
				info.Registrar = strings.TrimSpace(line[len("registrar:"):])
			}

		case strings.HasPrefix(lower, "registrant organization:"):
			if info.Registrant == "" {
				info.Registrant = strings.TrimSpace(line[len("registrant organization:"):])
			}

		case strings.HasPrefix(lower, "registrant name:"):
			if info.Registrant == "" {
				info.Registrant = strings.TrimSpace(line[len("registrant name:"):])
			}

		case strings.HasPrefix(lower, "creation date:"),
			strings.HasPrefix(lower, "registered on:"):
			if info.CreationDate == "" {
				if idx := strings.Index(line, ":"); idx != -1 {
					info.CreationDate = strings.TrimSpace(line[idx+1:])
				}
			}

		case strings.HasPrefix(lower, "registry expiry date:"),
			strings.HasPrefix(lower, "expiry date:"):
			if info.ExpirationDate == "" {
				if idx := strings.Index(line, ":"); idx != -1 {
					info.ExpirationDate = strings.TrimSpace(line[idx+1:])
				}
			}

		case strings.HasPrefix(lower, "updated date:"),
			strings.HasPrefix(lower, "last updated on:"):
			if info.UpdatedDate == "" {
				if idx := strings.Index(line, ":"); idx != -1 {
					info.UpdatedDate = strings.TrimSpace(line[idx+1:])
				}
			}

		case strings.HasPrefix(lower, "name server:"):
			v := strings.TrimSpace(line[len("name server:"):])
			if v != "" {
				nameServers = append(nameServers, v)
			}

		case strings.HasPrefix(lower, "nserver:"):
			v := strings.TrimSpace(line[len("nserver:"):])
			if v != "" {
				fields := strings.Fields(v)
				if len(fields) > 0 {
					nameServers = append(nameServers, fields[0])
				}
			}
		}

		// извлечение e-mail
		if strings.Contains(line, "@") {
			for _, field := range strings.Fields(line) {
				if !strings.Contains(field, "@") {
					continue
				}
				addr := strings.Trim(field, " ,;()<>")
				if strings.Count(addr, "@") != 1 {
					continue
				}
				if len(addr) < 3 {
					continue
				}
				emailSet[addr] = struct{}{}
			}
		}
	}

	// Дедупликация NS и e-mail'ов
	if len(nameServers) > 0 {
		seen := map[string]struct{}{}
		for _, ns := range nameServers {
			ns = strings.ToLower(strings.TrimSpace(ns))
			if ns == "" {
				continue
			}
			if _, ok := seen[ns]; ok {
				continue
			}
			seen[ns] = struct{}{}
			info.NameServers = append(info.NameServers, ns)
		}
	}

	for e := range emailSet {
		info.Emails = append(info.Emails, e)
	}
	sort.Strings(info.NameServers)
	sort.Strings(info.Emails)

	return info
}
