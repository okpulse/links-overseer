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

// WhoisInfo представляет собой агрегированную информацию WHOIS для домена.
type WhoisInfo struct {
	Domain         string   `json:"domain"`
	IP             string   `json:"ip"`
	Registrar      string   `json:"registrar"`
	Registrant     string   `json:"registrant"`
	OrgName        string   `json:"org_name"`
	OrgAddress     string   `json:"org_address"`
	CreationDate   string   `json:"creation_date"`
	UpdatedDate    string   `json:"updated_date"`
	ExpirationDate string   `json:"expiration_date"`
	NameServers    []string `json:"name_servers"`
	Emails         []string `json:"emails"`
	Raw            string   `json:"raw"`
}

// FetchWhois делает WHOIS-запрос для указанного домена или URL и возвращает агрегированную структуру.
func FetchWhois(ctx context.Context, target string) (*WhoisInfo, error) {
	domain := normalizeDomain(target)
	if domain == "" {
		return nil, errors.New("empty domain")
	}

	ip := ""
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", domain)
	if err == nil && len(ips) > 0 {
		for _, addr := range ips {
			if v4 := addr.To4(); v4 != nil {
				ip = v4.String()
				break
			}
		}
		if ip == "" {
			ip = ips[0].String()
		}
	}

	// 2) WHOIS
	raw, err := fetchWhoisRaw(ctx, domain)
	if err != nil {
		return nil, err
	}

	info := parseWhois(domain, raw)
	info.IP = ip
	return &info, nil
}

// normalizeDomain приводит введённую строку (URL или домен) к доменному имени.
func normalizeDomain(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}

	// Если в строке есть схема, пробуем распарсить как URL.
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		host = strings.TrimPrefix(host, "www.")
		return host
	}

	// Иначе считаем, что это доменное имя или "домен + путь".
	target = strings.ToLower(target)
	target = strings.TrimPrefix(target, "www.")

	// Отрезаем всё, что похоже на путь или query-параметры.
	if i := strings.IndexAny(target, "/?"); i != -1 {
		target = target[:i]
	}

	return target
}

// fetchWhoisRaw выполняет запрос к IANA, а затем к конкретному WHOIS-серверу (если он указан).
func fetchWhoisRaw(ctx context.Context, domain string) (string, error) {
	// Сначала IANA, чтобы узнать авторитетный WHOIS-сервер.
	ianaRaw, err := whoisQuery(ctx, "whois.iana.org", domain)
	if err != nil {
		return "", err
	}

	server := parseWhoisServer(ianaRaw)
	if server == "" {
		// Не удалось найти сервер — вернём хотя бы ответ IANA.
		return ianaRaw, nil
	}

	// Запрос к конкретному WHOIS-серверу.
	raw, err := whoisQuery(ctx, server, domain)
	if err != nil {
		// Если не получилось, вернём IANA-ответ.
		return ianaRaw, nil
	}
	return raw, nil
}

// whoisQuery делает TCP-запрос на порт 43 к указанному серверу.
func whoisQuery(ctx context.Context, server, query string) (string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(server, "43"))
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, query+"\r\n"); err != nil {
		return "", err
	}

	buf := &strings.Builder{}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		buf.WriteString(scanner.Text())
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// parseWhoisServer пытается вытащить адрес WHOIS-сервера из ответа IANA.
func parseWhoisServer(raw string) string {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "%") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "whois:") {
			return strings.TrimSpace(line[len("whois:"):])
		}
	}
	return ""
}

// parseWhois разбирает текст WHOIS в более удобную структуру.
func parseWhois(domain, raw string) WhoisInfo {
	info := WhoisInfo{
		Domain: strings.ToLower(domain),
		Raw:    raw,
	}

	var nameServers []string
	emailSet := map[string]struct{}{}
	var addressParts []string

	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Комментарии разных WHOIS-серверов
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "%") {
			continue
		}

		lower := strings.ToLower(line)

		// Базовые поля.
		switch {
		case strings.HasPrefix(lower, "domain name:"):
			v := strings.TrimSpace(line[len("domain name:"):])
			if v != "" {
				info.Domain = v
			}

		case strings.HasPrefix(lower, "registrar:"):
			if info.Registrar == "" {
				info.Registrar = strings.TrimSpace(line[len("registrar:"):])
			}

		// Организация / владелец (разные варианты полей)
		case strings.HasPrefix(lower, "registrant organization:"):
			val := strings.TrimSpace(line[len("registrant organization:"):])
			if info.Registrant == "" {
				info.Registrant = val
			}
			if info.OrgName == "" {
				info.OrgName = val
			}

		case strings.HasPrefix(lower, "registrant name:"):
			val := strings.TrimSpace(line[len("registrant name:"):])
			if info.Registrant == "" {
				info.Registrant = val
			}
			if info.OrgName == "" {
				info.OrgName = val
			}

		case strings.HasPrefix(lower, "org:"):
			val := strings.TrimSpace(line[len("org:"):])
			if info.OrgName == "" {
				info.OrgName = val
			}

		case strings.HasPrefix(lower, "org-name:"):
			val := strings.TrimSpace(line[len("org-name:"):])
			if info.OrgName == "" {
				info.OrgName = val
			}

		// Даты регистрации / истечения / обновления
		case strings.HasPrefix(lower, "creation date:"),
			strings.HasPrefix(lower, "created on:"),
			strings.HasPrefix(lower, "created:"),
			strings.HasPrefix(lower, "domain registration date:"),
			strings.HasPrefix(lower, "registered on:"):
			if info.CreationDate == "" {
				if idx := strings.Index(line, ":"); idx != -1 {
					info.CreationDate = strings.TrimSpace(line[idx+1:])
				}
			}

		case strings.HasPrefix(lower, "registry expiry date:"),
			strings.HasPrefix(lower, "registrar registration expiry date:"),
			strings.HasPrefix(lower, "registrar registration expiration date:"),
			strings.HasPrefix(lower, "expiry date:"),
			strings.HasPrefix(lower, "expire-date:"),
			strings.HasPrefix(lower, "paid-till:"),
			strings.HasPrefix(lower, "paid till:"),
			strings.HasPrefix(lower, "valid until:"),
			strings.HasPrefix(lower, "renewal date:"):
			if info.ExpirationDate == "" {
				if idx := strings.Index(line, ":"); idx != -1 {
					info.ExpirationDate = strings.TrimSpace(line[idx+1:])
				}
			}

		case strings.HasPrefix(lower, "updated date:"),
			strings.HasPrefix(lower, "last updated on:"),
			strings.HasPrefix(lower, "last updated:"),
			strings.HasPrefix(lower, "changed:"),
			strings.HasPrefix(lower, "modified:"):
			if info.UpdatedDate == "" {
				if idx := strings.Index(line, ":"); idx != -1 {
					info.UpdatedDate = strings.TrimSpace(line[idx+1:])
				}
			}

		// Name servers
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

		// Адрес организации (разные варианты полей у регистранта)
		if strings.HasPrefix(lower, "registrant street:") ||
			strings.HasPrefix(lower, "registrant city:") ||
			strings.HasPrefix(lower, "registrant state/province:") ||
			strings.HasPrefix(lower, "registrant postal code:") ||
			strings.HasPrefix(lower, "registrant country:") ||
			strings.HasPrefix(lower, "registrant address:") {
			if idx := strings.Index(line, ":"); idx != -1 {
				val := strings.TrimSpace(line[idx+1:])
				if val != "" {
					addressParts = append(addressParts, val)
				}
			}
		}

		// Простейшее извлечение e-mail'ов: ищем токены с '@'
		fields := strings.Fields(line)
		for _, token := range fields {
			if strings.Contains(token, "@") {
				// Чистим от возможных скобок и запятых.
				clean := strings.Trim(token, " <>\";,()")
				if strings.Contains(clean, "@") {
					emailSet[clean] = struct{}{}
				}
			}
		}
	}

	// Собираем адрес организации, если нашли части.
	if len(addressParts) > 0 && info.OrgAddress == "" {
		joined := strings.Join(addressParts, ", ")
		low := strings.TrimSpace(strings.ToLower(joined))
		if low != "" && low != "n/a" && low != "n/a, n/a" && low != "n/a, n/a, n/a" {
			info.OrgAddress = joined
		}
	}

	// Дедупликация NS и e-mail'ов.
	seen := map[string]struct{}{}
	for _, ns := range nameServers {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		info.NameServers = append(info.NameServers, ns)
	}

	for e := range emailSet {
		info.Emails = append(info.Emails, e)
	}
	sort.Strings(info.NameServers)
	sort.Strings(info.Emails)

	return info
}
