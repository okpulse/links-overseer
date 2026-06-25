package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/go-pdf/fpdf"
	"github.com/okpulse/links-overseer/internal/core"
)

const maxDocumentSizeBytes = 300 * 1024 * 1024

const browserLikeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

var emailRegex = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)

//go:embed fonts/DejaVuSans.ttf
var bundledPDFReportFont []byte

type DocumentItem struct {
	ID                 int64  `json:"id"`
	FileURL            string `json:"fileUrl"`
	PageURL            string `json:"pageUrl"`
	FileName           string `json:"fileName"`
	FileType           string `json:"fileType"`
	TypeGroup          string `json:"typeGroup"`
	SizeBytes          int64  `json:"sizeBytes"`
	MimeType           string `json:"mimeType"`
	Status             string `json:"status"`
	MetadataShort      string `json:"metadataShort"`
	HasMetadata        bool   `json:"hasMetadata"`
	Downloaded         bool   `json:"downloaded"`
	TooLarge           bool   `json:"tooLarge"`
	DownloadError      string `json:"downloadError,omitempty"`
	LocalPath          string `json:"localPath,omitempty"`
	MD5                string `json:"md5,omitempty"`
	SHA1               string `json:"sha1,omitempty"`
	SHA256             string `json:"sha256,omitempty"`
	Title              string `json:"title,omitempty"`
	Author             string `json:"author,omitempty"`
	Company            string `json:"company,omitempty"`
	Creator            string `json:"creator,omitempty"`
	Producer           string `json:"producer,omitempty"`
	LastModifiedBy     string `json:"lastModifiedBy,omitempty"`
	Created            string `json:"created,omitempty"`
	Modified           string `json:"modified,omitempty"`
	Pages              int    `json:"pages,omitempty"`
	MetadataSource     string `json:"metadataSource,omitempty"`
	MetadataRawSummary string `json:"metadataRawSummary,omitempty"`
	Contacts           string `json:"contacts,omitempty"`
}

type docMeta struct {
	Title          string
	Author         string
	Company        string
	Creator        string
	Producer       string
	LastModifiedBy string
	Created        string
	Modified       string
	Pages          int
	Source         string
	Contacts       []string
}

func addDocumentToJob(job *Job, ref core.DocumentRef) {
	if ref.URL == "" {
		return
	}
	if isJobCanceled(job) {
		return
	}

	ft := normalizeDocType(ref.FileType)
	name := resolveDocumentDisplayName(ref.FileName, ref.URL, ft)
	if ft == "" {
		if t, ok := core.DetectDocumentType(ref.URL); ok {
			ft = t
		}
	}
	if ft == "" {
		return
	}

	job.mu.Lock()
	for i := range job.Documents {
		if strings.EqualFold(job.Documents[i].FileURL, ref.URL) && strings.EqualFold(job.Documents[i].PageURL, ref.PageURL) {
			job.mu.Unlock()
			return
		}
	}
	job.nextDocID++
	id := job.nextDocID
	item := DocumentItem{
		ID:        id,
		FileURL:   ref.URL,
		PageURL:   ref.PageURL,
		FileName:  name,
		FileType:  ft,
		TypeGroup: documentGroup(ft),
		Status:    "найден",
	}
	job.Documents = append(job.Documents, item)
	job.mu.Unlock()

	if job.Params.DownloadDocuments {
		if _, err := downloadDocumentForJobCtx(job, id, operationContext(job), true); err != nil {
			if isCancellationErr(err) || isJobCanceled(job) {
				job.mu.Lock()
				for i := range job.Documents {
					if job.Documents[i].ID == id {
						job.Documents[i].Status = "остановлен"
						job.Documents[i].DownloadError = ""
						break
					}
				}
				job.mu.Unlock()
				return
			}
			job.mu.Lock()
			for i := range job.Documents {
				if job.Documents[i].ID == id {
					job.Documents[i].Status = "ошибка скачивания"
					job.Documents[i].DownloadError = err.Error()
					break
				}
			}
			job.mu.Unlock()
		}
	}
}

func normalizeDocType(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt":
		return v
	default:
		return ""
	}
}

func documentGroup(ft string) string {
	switch normalizeDocType(ft) {
	case "doc", "docx":
		return "doc"
	case "xls", "xlsx":
		return "xls"
	case "ppt", "pptx":
		return "ppt"
	case "pdf":
		return "pdf"
	case "txt":
		return "txt"
	default:
		return "other"
	}
}

func handleDocuments(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)

	job.mu.Lock()
	docs := make([]DocumentItem, len(job.Documents))
	copy(docs, job.Documents)
	job.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(docs)
}

func handleDocumentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}

	jobID := r.URL.Query().Get("job")
	idStr := r.URL.Query().Get("id")
	if jobID == "" || idStr == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	v, ok := jobs.Load(jobID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)

	item, err := downloadDocumentForJob(job, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(item)
}

func downloadDocumentForJob(job *Job, id int64) (*DocumentItem, error) {
	return downloadDocumentForJobCtx(job, id, context.Background(), false)
}

func downloadDocumentForJobCtx(job *Job, id int64, ctx context.Context, stopAware bool) (*DocumentItem, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if stopAware && isJobCanceled(job) {
		return nil, context.Canceled
	}
	job.mu.Lock()
	idx := -1
	for i := range job.Documents {
		if job.Documents[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		job.mu.Unlock()
		return nil, fmt.Errorf("document not found")
	}
	if job.Documents[idx].TooLarge {
		itemCopy := job.Documents[idx]
		job.mu.Unlock()
		return &itemCopy, nil
	}
	fileURL := job.Documents[idx].FileURL
	pageURL := job.Documents[idx].PageURL
	startURL := job.Params.StartURL
	job.mu.Unlock()

	u, err := url.Parse(startURL)
	if err != nil {
		return nil, fmt.Errorf("bad start url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		host = "site"
	}
	dir := filepath.Join("data", host, "documents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	data, mimeType, finalURL, suggestedName, err := fetchDocumentBytes(ctx, fileURL, pageURL, startURL)
	if err != nil {
		return nil, err
	}
	if stopAware && isJobCanceled(job) {
		return nil, context.Canceled
	}
	size := int64(len(data))

	var buf bytes.Buffer
	buf.Write(data)
	n := size
	if n > maxDocumentSizeBytes {
		job.mu.Lock()
		job.Documents[idx].TooLarge = true
		job.Documents[idx].Status = "слишком большой"
		job.Documents[idx].SizeBytes = n
		itemCopy := job.Documents[idx]
		job.mu.Unlock()
		return &itemCopy, nil
	}
	data = buf.Bytes()
	if strings.TrimSpace(mimeType) == "" {
		mimeType = http.DetectContentType(data)
	}

	job.mu.Lock()
	ft := job.Documents[idx].FileType
	name := job.Documents[idx].FileName
	job.mu.Unlock()

	ft = refineDocumentType(ft, name, mimeType, data)
	name = resolveDownloadedDocumentName(name, suggestedName, finalURL, ft)
	name = sanitizeDocumentFileName(name, id, ft)
	if stopAware && isJobCanceled(job) {
		return nil, context.Canceled
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	meta, hasMeta, summary := extractDocumentMetadata(ft, data)
	md5sum := checksumMD5(data)
	sha1sum := checksumSHA1(data)
	sha256sum := checksumSHA256(data)

	job.mu.Lock()
	job.Documents[idx].FileType = ft
	job.Documents[idx].TypeGroup = documentGroup(ft)
	job.Documents[idx].FileName = name
	job.Documents[idx].MimeType = mimeType
	if strings.TrimSpace(finalURL) != "" {
		job.Documents[idx].FileURL = finalURL
	}
	job.Documents[idx].SizeBytes = size
	job.Documents[idx].Downloaded = true
	job.Documents[idx].TooLarge = false
	job.Documents[idx].Status = "скачан"
	job.Documents[idx].LocalPath = path
	job.Documents[idx].MD5 = md5sum
	job.Documents[idx].SHA1 = sha1sum
	job.Documents[idx].SHA256 = sha256sum
	job.Documents[idx].HasMetadata = hasMeta
	job.Documents[idx].MetadataShort = summary
	job.Documents[idx].DownloadError = ""
	applyDocMeta(&job.Documents[idx], meta)
	if hasMeta {
		job.Documents[idx].Status = "метаданные извлечены"
	} else if job.Documents[idx].MetadataShort == "" {
		job.Documents[idx].MetadataShort = "метаданные не обнаружены"
	}
	itemCopy := job.Documents[idx]
	job.mu.Unlock()

	return &itemCopy, nil
}

func fetchDocumentBytes(ctx context.Context, fileURL, pageURL, startURL string) ([]byte, string, string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 45 * time.Second, Jar: jar}

	if pageURL != "" {
		if _, _, _, _, err := doDocumentRequest(ctx, client, pageURL, pageURL); err != nil {
			// cookies
		}
	}

	data, mimeType, finalURL, suggestedName, err := doDocumentRequest(ctx, client, fileURL, pageURL)
	if err == nil {
		return data, mimeType, finalURL, suggestedName, nil
	}
	if pageURL != "" && pageURL != startURL {
		if _, _, _, _, err2 := doDocumentRequest(ctx, client, startURL, startURL); err2 == nil {
			data, mimeType, finalURL, suggestedName, err = doDocumentRequest(ctx, client, fileURL, pageURL)
			if err == nil {
				return data, mimeType, finalURL, suggestedName, nil
			}
		}
	}
	return nil, "", "", "", err
}

func doDocumentRequest(ctx context.Context, client *http.Client, targetURL, referer string) ([]byte, string, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("bad request: %w", err)
	}
	setDocumentHeaders(req, referer)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("get document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := readPossiblyCompressedBody(ctx, resp, maxDocumentSizeBytes)
	if err != nil {
		return nil, "", "", "", err
	}
	return body, strings.TrimSpace(resp.Header.Get("Content-Type")), resp.Request.URL.String(), parseContentDispositionFilename(resp.Header.Get("Content-Disposition")), nil
}

func setDocumentHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", browserLikeUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,ru;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	if strings.TrimSpace(referer) != "" {
		req.Header.Set("Referer", referer)
	}
}

func readPossiblyCompressedBody(ctx context.Context, resp *http.Response, limit int64) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reader io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		nr, er := reader.Read(chunk)
		if nr > 0 {
			remaining := limit + 1 - total
			if remaining <= 0 {
				break
			}
			if int64(nr) > remaining {
				nr = int(remaining)
			}
			if _, ew := buf.Write(chunk[:nr]); ew != nil {
				return nil, fmt.Errorf("read: %w", ew)
			}
			total += int64(nr)
			if total > limit {
				break
			}
		}
		if er != nil {
			if er == io.EOF {
				break
			}
			return nil, fmt.Errorf("read: %w", er)
		}
	}
	return buf.Bytes(), nil
}

func applyDocMeta(item *DocumentItem, meta docMeta) {
	item.Title = meta.Title
	item.Author = meta.Author
	item.Company = meta.Company
	item.Creator = meta.Creator
	item.Producer = meta.Producer
	item.LastModifiedBy = meta.LastModifiedBy
	item.Created = meta.Created
	item.Modified = meta.Modified
	item.Pages = meta.Pages
	item.MetadataSource = meta.Source
	item.Contacts = strings.Join(meta.Contacts, ", ")
	item.MetadataRawSummary = strings.Join(nonEmptyStrings(
		nonEmptyLabel("Title", meta.Title),
		nonEmptyLabel("Author", meta.Author),
		nonEmptyLabel("Company", meta.Company),
		nonEmptyLabel("Creator", meta.Creator),
		nonEmptyLabel("Producer", meta.Producer),
		nonEmptyLabel("LastModifiedBy", meta.LastModifiedBy),
		nonEmptyLabel("Created", meta.Created),
		nonEmptyLabel("Modified", meta.Modified),
		nonEmptyLabel("Contacts", item.Contacts),
	), " | ")
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}
func nonEmptyLabel(label, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return label + ": " + value
}

func joinContacts(values []string) string {
	return strings.Join(uniqueNonEmptyStrings(values), ", ")
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out
}

func extractEmailsFromStrings(values ...string) []string {
	var out []string
	for _, v := range values {
		matches := emailRegex.FindAllString(v, -1)
		for _, m := range matches {
			out = append(out, strings.TrimSpace(m))
		}
	}
	return uniqueNonEmptyStrings(out)
}

func normalizedMetaKey(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(v)
	return v
}

func isContactMetaKey(v string) bool {
	switch normalizedMetaKey(v) {
	case "email", "eemail", "emailaddress", "authoremail", "contact", "contactemail", "manageremail", "companyemail", "mailto":
		return true
	default:
		return false
	}
}

func checksumMD5(data []byte) string    { s := md5.Sum(data); return hex.EncodeToString(s[:]) }
func checksumSHA1(data []byte) string   { s := sha1.Sum(data); return hex.EncodeToString(s[:]) }
func checksumSHA256(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }

func resolveDocumentDisplayName(linkText, rawURL, ft string) string {
	linkText = strings.TrimSpace(linkText)
	urlName := strings.TrimSpace(core.GuessFileNameFromURL(rawURL))
	if isUsableDocumentName(urlName, ft) {
		return urlName
	}
	if isUsableDocumentName(linkText, ft) {
		return ensureDocumentNameExtension(linkText, ft)
	}
	if urlName != "" {
		return ensureDocumentNameExtension(urlName, ft)
	}
	if linkText != "" {
		return ensureDocumentNameExtension(linkText, ft)
	}
	if ft != "" {
		return "document." + ft
	}
	return "document"
}

func resolveDownloadedDocumentName(currentName, suggestedName, finalURL, ft string) string {
	if isUsableDocumentName(suggestedName, ft) {
		return ensureDocumentNameExtension(strings.TrimSpace(suggestedName), ft)
	}
	urlName := strings.TrimSpace(core.GuessFileNameFromURL(finalURL))
	if isUsableDocumentName(urlName, ft) {
		return urlName
	}
	if isUsableDocumentName(currentName, ft) {
		return ensureDocumentNameExtension(strings.TrimSpace(currentName), ft)
	}
	if urlName != "" {
		return ensureDocumentNameExtension(urlName, ft)
	}
	if currentName != "" {
		return ensureDocumentNameExtension(strings.TrimSpace(currentName), ft)
	}
	if ft != "" {
		return "document." + ft
	}
	return "document"
}

func isUsableDocumentName(name, ft string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	generic := map[string]struct{}{
		"download": {}, "download file": {}, "скачать": {}, "загрузить": {}, "открыть": {}, "open": {}, "file": {}, "document": {}, "документ": {},
	}
	if _, ok := generic[lower]; ok {
		return false
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if ft != "" && ext == ft {
		return true
	}
	if ext != "" {
		return true
	}
	return false
}

func ensureDocumentNameExtension(name, ft string) string {
	name = strings.TrimSpace(name)
	if name == "" || ft == "" {
		return name
	}
	want := "." + strings.ToLower(strings.TrimSpace(ft))
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, want) {
		return name
	}
	if strings.HasSuffix(lower, strings.TrimPrefix(want, ".")) {
		return name[:len(name)-len(want)+1] + want
	}
	if filepath.Ext(name) != "" {
		return name
	}
	return name + want
}

func parseContentDispositionFilename(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	if v := strings.TrimSpace(params["filename*"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(params["filename"]); v != "" {
		return v
	}
	return ""
}

func sanitizeDocumentFileName(name string, id int64, ft string) string {

	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.ReplaceAll(name, "\r", " ")
	name = strings.ReplaceAll(name, "	", " ")

	ext := strings.ToLower(filepath.Ext(name))
	base := strings.TrimSpace(strings.TrimSuffix(name, filepath.Ext(name)))
	if ext == "" && ft != "" {
		ext = "." + strings.ToLower(strings.TrimSpace(ft))
	}
	if ext != "" {
		ext = sanitizeDocumentExt(ext)
		if ext == "" || ext == "." {
			ext = ".bin"
		}
	}
	base = sanitizeWindowsNamePart(base, true)
	if base == "" {
		base = fmt.Sprintf("document_%d", id)
	}
	if isWindowsReservedName(base) {
		base = "file_" + base
	}

	const maxBaseRunes = 120
	if utf8.RuneCountInString(base) > maxBaseRunes {
		r := []rune(base)
		base = string(r[:maxBaseRunes])
		base = strings.TrimRight(base, " ._-")
		if base == "" {
			base = fmt.Sprintf("document_%d", id)
		}
	}

	return base + ext
}

func sanitizeDocumentExt(ext string) string {
	ext = strings.TrimSpace(ext)
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	clean := sanitizeWindowsNamePart(strings.TrimPrefix(ext, "."), false)
	if clean == "" {
		return ""
	}
	return "." + clean
}

func sanitizeWindowsNamePart(s string, replaceSpace bool) string {

	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < 32:
			continue
		case strings.ContainsRune(`<>:"/\\|?*`, r):
			b.WriteRune('_')
		case replaceSpace && (r == '\n' || r == '\r' || r == '	'):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".")
	out = strings.Join(strings.Fields(out), " ")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	out = strings.Trim(out, " .")
	return out
}

func isWindowsReservedName(name string) bool {
	name = strings.TrimSpace(strings.Trim(name, "."))
	if name == "" {
		return false
	}
	upper := strings.ToUpper(name)
	switch upper {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return true
	}
	for i := 1; i <= 9; i++ {
		if upper == fmt.Sprintf("COM%d", i) || upper == fmt.Sprintf("LPT%d", i) {
			return true
		}
	}
	return false
}

func refineDocumentType(ft, name, mimeType string, data []byte) string {
	ft = normalizeDocType(ft)
	if ft == "" {
		ft = normalizeDocType(strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), "."))
	}
	lowerMime := strings.ToLower(mimeType)
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(data, []byte{'P', 'K', 3, 4}):
		if isOOXMLType(data, "word/") {
			return "docx"
		}
		if isOOXMLType(data, "xl/") {
			return "xlsx"
		}
		if isOOXMLType(data, "ppt/") {
			return "pptx"
		}
	case strings.Contains(lowerMime, "pdf"):
		return "pdf"
	case strings.Contains(lowerMime, "wordprocessingml"):
		return "docx"
	case strings.Contains(lowerMime, "spreadsheetml"):
		return "xlsx"
	case strings.Contains(lowerMime, "presentationml"):
		return "pptx"
	case strings.Contains(lowerMime, "msword"):
		return "doc"
	case strings.Contains(lowerMime, "excel"):
		return "xls"
	case strings.Contains(lowerMime, "powerpoint"):
		return "ppt"
	case strings.Contains(lowerMime, "text/plain"):
		return "txt"
	}
	return ft
}

func isOOXMLType(data []byte, prefix string) bool {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			return true
		}
	}
	return false
}

func extractDocumentMetadata(ft string, data []byte) (docMeta, bool, string) {
	ft = normalizeDocType(ft)
	switch ft {
	case "pdf":
		return extractPDFMetadata(data)
	case "docx", "xlsx", "pptx":
		return extractOOXMLMetadata(data)
	case "txt":
		return docMeta{Source: "txt"}, false, "текстовый файл: служебные метаданные обычно отсутствуют"
	case "doc", "xls", "ppt":
		return extractLegacyOfficeMetadata(ft, data)
	default:
		return docMeta{}, false, "метаданные не обнаружены"
	}
}

func extractPDFMetadata(data []byte) (docMeta, bool, string) {
	meta := docMeta{Source: "pdf"}
	text := string(data)
	pairs := map[string]*string{
		"Title":    &meta.Title,
		"Author":   &meta.Author,
		"Creator":  &meta.Creator,
		"Producer": &meta.Producer,
	}
	for key, target := range pairs {
		*target = extractPDFInfoString(text, key)
	}
	contactValues := []string{}
	for _, key := range []string{"Email", "E-mail", "AuthorEmail", "Contact", "ContactEmail", "ManagerEmail", "CompanyEmail"} {
		if v := extractPDFInfoString(text, key); v != "" {
			contactValues = append(contactValues, key+": "+v)
		}
	}
	emails := extractEmailsFromStrings(meta.Title, meta.Author, meta.Creator, meta.Producer, strings.Join(contactValues, " | "))
	meta.Contacts = append(meta.Contacts, uniqueNonEmptyStrings(contactValues)...)
	meta.Contacts = append(meta.Contacts, emails...)
	meta.Contacts = uniqueNonEmptyStrings(meta.Contacts)
	meta.Pages = strings.Count(text, "/Type /Page")
	meta.Created = extractPDFDateField(text, "CreationDate")
	meta.Modified = extractPDFDateField(text, "ModDate")
	parts := []string{}
	if meta.Author != "" {
		parts = append(parts, "Author: "+meta.Author)
	}
	if meta.Creator != "" {
		parts = append(parts, "Creator: "+meta.Creator)
	}
	if meta.Producer != "" {
		parts = append(parts, "Producer: "+meta.Producer)
	}
	if len(meta.Contacts) > 0 {
		parts = append(parts, "Contacts: "+joinContacts(meta.Contacts))
	}
	if meta.Pages > 0 {
		parts = append(parts, fmt.Sprintf("Pages: %d", meta.Pages))
	}
	summary := strings.Join(parts, " | ")
	has := meta.Title != "" || meta.Author != "" || meta.Creator != "" || meta.Producer != "" || meta.Created != "" || meta.Modified != "" || meta.Pages > 0 || len(meta.Contacts) > 0
	if summary == "" {
		summary = "PDF скачан; явные метаданные не обнаружены"
	}
	return meta, has, summary
}

func extractPDFInfoString(text, key string) string {
	marker := "/" + key
	idx := strings.Index(text, marker)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(marker):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if rest == "" {
		return ""
	}
	if rest[0] == '(' {
		raw, ok := parsePDFLiteralString(rest)
		if !ok {
			return ""
		}
		return decodePDFString(raw)
	}
	if rest[0] == '<' && len(rest) > 1 && rest[1] != '<' {
		raw, ok := parsePDFHexString(rest)
		if !ok {
			return ""
		}
		return decodePDFString(raw)
	}
	return ""
}

func parsePDFLiteralString(s string) ([]byte, bool) {
	if s == "" || s[0] != '(' {
		return nil, false
	}
	var out []byte
	depth := 0
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 {
			depth = 1
			continue
		}
		if escaped {
			switch c {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '(', ')', '\\':
				out = append(out, c)
			case '\n':
				// line continuation
			case '\r':
				if i+1 < len(s) && s[i+1] == '\n' {
					i++
				}
			default:
				if c >= '0' && c <= '7' {
					val := int(c - '0')
					count := 1
					for count < 3 && i+1 < len(s) {
						n := s[i+1]
						if n < '0' || n > '7' {
							break
						}
						val = val*8 + int(n-'0')
						i++
						count++
					}
					out = append(out, byte(val))
				} else {
					out = append(out, c)
				}
			}
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '(' {
			depth++
			out = append(out, c)
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return out, true
			}
			out = append(out, c)
			continue
		}
		out = append(out, c)
	}
	return nil, false
}

func parsePDFHexString(s string) ([]byte, bool) {
	if len(s) < 2 || s[0] != '<' || s[1] == '<' {
		return nil, false
	}
	end := strings.IndexByte(s[1:], '>')
	if end < 0 {
		return nil, false
	}
	hexPart := s[1 : 1+end]
	hexPart = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t', '\f', '\v':
			return -1
		default:
			return r
		}
	}, hexPart)
	if len(hexPart)%2 == 1 {
		hexPart += "0"
	}
	b, err := hex.DecodeString(hexPart)
	if err != nil {
		return nil, false
	}
	return b, true
}

func decodePDFString(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) >= 2 {
		if raw[0] == 0xFE && raw[1] == 0xFF {
			return cleanPDFDecodedString(decodeUTF16BERunes(raw[2:]))
		}
		if raw[0] == 0xFF && raw[1] == 0xFE {
			return cleanPDFDecodedString(decodeUTF16LERunes(raw[2:]))
		}
	}
	zeroPairs := 0
	for i := 0; i+1 < len(raw); i += 2 {
		if raw[i] == 0x00 || raw[i+1] == 0x00 {
			zeroPairs++
		}
	}
	if len(raw) >= 4 && zeroPairs >= len(raw)/4 {
		return cleanPDFDecodedString(decodeUTF16BERunes(raw))
	}
	return cleanPDFDecodedString(string(raw))
}

func decodeUTF16BERunes(raw []byte) string {
	if len(raw)%2 == 1 {
		raw = raw[:len(raw)-1]
	}
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u16 = append(u16, uint16(raw[i])<<8|uint16(raw[i+1]))
	}
	return string(utf16.Decode(u16))
}

func decodeUTF16LERunes(raw []byte) string {
	if len(raw)%2 == 1 {
		raw = raw[:len(raw)-1]
	}
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u16 = append(u16, uint16(raw[i+1])<<8|uint16(raw[i]))
	}
	return string(utf16.Decode(u16))
}

func cleanPDFDecodedString(v string) string {
	v = strings.ReplaceAll(v, "\x00", "")
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\t", " ")
	v = strings.Join(strings.Fields(v), " ")
	return strings.TrimSpace(v)
}

func extractPDFDateField(text, key string) string {
	marker := "/" + key
	idx := strings.Index(text, marker)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(marker):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if rest == "" {
		return ""
	}
	if rest[0] == '(' {
		raw, ok := parsePDFLiteralString(rest)
		if !ok {
			return ""
		}
		val := strings.TrimSpace(decodePDFString(raw))
		return strings.TrimPrefix(val, "D:")
	}
	if rest[0] == '<' && len(rest) > 1 && rest[1] != '<' {
		raw, ok := parsePDFHexString(rest)
		if !ok {
			return ""
		}
		val := strings.TrimSpace(decodePDFString(raw))
		return strings.TrimPrefix(val, "D:")
	}
	return ""
}

type ooXMLCoreProps struct {
	XMLName        xml.Name `xml:"coreProperties"`
	Title          string   `xml:"title"`
	Subject        string   `xml:"subject"`
	Creator        string   `xml:"creator"`
	LastModifiedBy string   `xml:"lastModifiedBy"`
	Created        string   `xml:"created"`
	Modified       string   `xml:"modified"`
	Description    string   `xml:"description"`
	Keywords       string   `xml:"keywords"`
}

type ooXMLAppProps struct {
	XMLName            xml.Name `xml:"Properties"`
	Company            string   `xml:"Company"`
	Application        string   `xml:"Application"`
	AppVersion         string   `xml:"AppVersion"`
	Pages              int      `xml:"Pages"`
	Slides             int      `xml:"Slides"`
	Words              int      `xml:"Words"`
	PresentationFormat string   `xml:"PresentationFormat"`
}

type ooXMLCustomProperties struct {
	XMLName    xml.Name              `xml:"Properties"`
	Properties []ooXMLCustomProperty `xml:"property"`
}

type ooXMLCustomProperty struct {
	Name     string `xml:"name,attr"`
	Lpwstr   string `xml:"lpwstr"`
	Lpstr    string `xml:"lpstr"`
	Bstr     string `xml:"bstr"`
	Filetime string `xml:"filetime"`
	I4       string `xml:"i4"`
	I8       string `xml:"i8"`
	R8       string `xml:"r8"`
	Bool     string `xml:"bool"`
}

func (p ooXMLCustomProperty) Value() string {
	for _, v := range []string{p.Lpwstr, p.Lpstr, p.Bstr, p.Filetime, p.I4, p.I8, p.R8, p.Bool} {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func extractOOXMLMetadata(data []byte) (docMeta, bool, string) {
	meta := docMeta{Source: "ooxml"}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return meta, false, "не удалось разобрать OOXML"
	}
	var rawValues []string
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		switch name {
		case "docprops/core.xml":
			b, err := readZipFile(f)
			if err == nil {
				var cp ooXMLCoreProps
				if xml.Unmarshal(b, &cp) == nil {
					meta.Title = strings.TrimSpace(cp.Title)
					meta.Author = strings.TrimSpace(cp.Creator)
					meta.LastModifiedBy = strings.TrimSpace(cp.LastModifiedBy)
					meta.Created = strings.TrimSpace(cp.Created)
					meta.Modified = strings.TrimSpace(cp.Modified)
					rawValues = append(rawValues, meta.Title, meta.Author, meta.LastModifiedBy, meta.Created, meta.Modified, strings.TrimSpace(cp.Subject), strings.TrimSpace(cp.Description), strings.TrimSpace(cp.Keywords))
				}
			}
		case "docprops/app.xml":
			b, err := readZipFile(f)
			if err == nil {
				var ap ooXMLAppProps
				if xml.Unmarshal(b, &ap) == nil {
					meta.Company = strings.TrimSpace(ap.Company)
					meta.Creator = strings.TrimSpace(ap.Application)
					if ap.Pages > 0 {
						meta.Pages = ap.Pages
					}
					if meta.Pages == 0 && ap.Slides > 0 {
						meta.Pages = ap.Slides
					}
					rawValues = append(rawValues, meta.Company, meta.Creator, strings.TrimSpace(ap.AppVersion), strings.TrimSpace(ap.PresentationFormat))
				}
			}
		case "docprops/custom.xml":
			b, err := readZipFile(f)
			if err == nil {
				var cp ooXMLCustomProperties
				if xml.Unmarshal(b, &cp) == nil {
					for _, prop := range cp.Properties {
						name := strings.TrimSpace(prop.Name)
						value := strings.TrimSpace(prop.Value())
						if name == "" || value == "" {
							continue
						}
						rawValues = append(rawValues, value, name+": "+value)
						if isContactMetaKey(name) {
							meta.Contacts = append(meta.Contacts, name+": "+value)
						}
					}
				}
			}
		}
	}
	meta.Contacts = append(meta.Contacts, extractEmailsFromStrings(rawValues...)...)
	meta.Contacts = uniqueNonEmptyStrings(meta.Contacts)
	parts := []string{}
	if meta.Author != "" {
		parts = append(parts, "Author: "+meta.Author)
	}
	if meta.LastModifiedBy != "" {
		parts = append(parts, "LastModifiedBy: "+meta.LastModifiedBy)
	}
	if meta.Company != "" {
		parts = append(parts, "Company: "+meta.Company)
	}
	if meta.Creator != "" {
		parts = append(parts, "App: "+meta.Creator)
	}
	if len(meta.Contacts) > 0 {
		parts = append(parts, "Contacts: "+joinContacts(meta.Contacts))
	}
	if meta.Pages > 0 {
		parts = append(parts, fmt.Sprintf("Count: %d", meta.Pages))
	}
	summary := strings.Join(parts, " | ")
	has := meta.Title != "" || meta.Author != "" || meta.Company != "" || meta.Creator != "" || meta.LastModifiedBy != "" || meta.Created != "" || meta.Modified != "" || meta.Pages > 0 || len(meta.Contacts) > 0
	if summary == "" {
		summary = "OOXML скачан; явные метаданные не обнаружены"
	}
	return meta, has, summary
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

type cfbHeader struct {
	SectorSize         int
	MiniSectorSize     int
	FirstDirSector     uint32
	MiniCutoff         uint32
	FirstMiniFATSector uint32
	NumMiniFATSectors  uint32
	FirstDIFATSector   uint32
	NumDIFATSectors    uint32
	DIFAT              []uint32
}

type cfbDirEntry struct {
	Name       string
	ObjectType byte
	Start      uint32
	Size       uint64
}

type cfbDocument struct {
	Header     cfbHeader
	Data       []byte
	FAT        []uint32
	MiniFAT    []uint32
	DirEntries []cfbDirEntry
	Root       *cfbDirEntry
	MiniStream []byte
}

const (
	cfbFreeSect   = 0xFFFFFFFF
	cfbEndOfChain = 0xFFFFFFFE
	cfbFatSect    = 0xFFFFFFFD
	cfbDifSect    = 0xFFFFFFFC
)

func extractLegacyOfficeMetadata(ft string, data []byte) (docMeta, bool, string) {
	meta := docMeta{Source: ft}
	doc, err := parseCFB(data)
	if err != nil {
		return meta, false, "старый Office-формат: не удалось разобрать контейнер OLE"
	}

	summaryValues := map[string]string{}
	if b, err := doc.ReadStream("\x05SummaryInformation"); err == nil {
		for k, v := range parseOLEPropertySetStream(b) {
			summaryValues[k] = v
		}
	}
	if b, err := doc.ReadStream("\x05DocumentSummaryInformation"); err == nil {
		for k, v := range parseOLEPropertySetStream(b) {
			summaryValues[k] = v
		}
	}

	meta.Title = firstNonEmptyMap(summaryValues, []string{"Title"})
	meta.Author = firstNonEmptyMap(summaryValues, []string{"Author"})
	meta.LastModifiedBy = firstNonEmptyMap(summaryValues, []string{"LastSavedBy", "Manager"})
	meta.Company = firstNonEmptyMap(summaryValues, []string{"Company"})
	meta.Creator = firstNonEmptyMap(summaryValues, []string{"ApplicationName", "AppName"})
	meta.Created = firstNonEmptyMap(summaryValues, []string{"CreateTime/Date", "Created"})
	meta.Modified = firstNonEmptyMap(summaryValues, []string{"LastSavedTime/Date", "Modified"})
	meta.Pages = parsePositiveInt(firstNonEmptyMap(summaryValues, []string{"PageCount", "Slides", "Count"}))

	for k, v := range summaryValues {
		if isContactMetaKey(k) {
			meta.Contacts = append(meta.Contacts, k+": "+v)
		}
	}
	allVals := make([]string, 0, len(summaryValues)*2)
	for k, v := range summaryValues {
		allVals = append(allVals, k, v, k+": "+v)
	}
	meta.Contacts = append(meta.Contacts, extractEmailsFromStrings(allVals...)...)
	meta.Contacts = uniqueNonEmptyStrings(meta.Contacts)

	parts := []string{}
	if meta.Author != "" {
		parts = append(parts, "Author: "+meta.Author)
	}
	if meta.LastModifiedBy != "" {
		parts = append(parts, "LastSavedBy: "+meta.LastModifiedBy)
	}
	if meta.Company != "" {
		parts = append(parts, "Company: "+meta.Company)
	}
	if meta.Creator != "" {
		parts = append(parts, "App: "+meta.Creator)
	}
	if len(meta.Contacts) > 0 {
		parts = append(parts, "Contacts: "+joinContacts(meta.Contacts))
	}
	if meta.Pages > 0 {
		parts = append(parts, fmt.Sprintf("Count: %d", meta.Pages))
	}
	summary := strings.Join(parts, " | ")
	has := meta.Title != "" || meta.Author != "" || meta.Company != "" || meta.Creator != "" || meta.LastModifiedBy != "" || meta.Created != "" || meta.Modified != "" || meta.Pages > 0 || len(meta.Contacts) > 0
	if summary == "" {
		summary = "старый Office-формат скачан; явные метаданные не обнаружены"
	}
	return meta, has, summary
}

func firstNonEmptyMap(m map[string]string, keys []string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

func parsePositiveInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	if n > 0 {
		return n
	}
	return 0
}

func parseCFB(data []byte) (*cfbDocument, error) {
	if len(data) < 512 || !bytes.Equal(data[:8], []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}) {
		return nil, fmt.Errorf("not cfb")
	}
	h, err := parseCFBHeader(data)
	if err != nil {
		return nil, err
	}
	fat, err := buildCFBFAT(data, h)
	if err != nil {
		return nil, err
	}
	doc := &cfbDocument{Header: h, Data: data, FAT: fat}
	dirBytes, err := doc.readSectorChain(h.FirstDirSector, 0)
	if err != nil {
		return nil, err
	}
	dirs := parseCFBDirectoryEntries(dirBytes)
	doc.DirEntries = dirs
	for i := range dirs {
		if dirs[i].ObjectType == 5 {
			doc.Root = &dirs[i]
			break
		}
	}
	if h.NumMiniFATSectors > 0 && h.FirstMiniFATSector != cfbEndOfChain {
		miniBytes, err := doc.readSectorChain(h.FirstMiniFATSector, int(h.NumMiniFATSectors)*h.SectorSize)
		if err == nil {
			doc.MiniFAT = bytesToUint32Slice(miniBytes)
		}
	}
	if doc.Root != nil && doc.Root.Size > 0 {
		rootStream, err := doc.readSectorChain(doc.Root.Start, int(doc.Root.Size))
		if err == nil {
			doc.MiniStream = rootStream
		}
	}
	return doc, nil
}

func parseCFBHeader(data []byte) (cfbHeader, error) {
	sectorShift := binary.LittleEndian.Uint16(data[30:32])
	miniSectorShift := binary.LittleEndian.Uint16(data[32:34])
	h := cfbHeader{
		SectorSize:         1 << sectorShift,
		MiniSectorSize:     1 << miniSectorShift,
		FirstDirSector:     binary.LittleEndian.Uint32(data[48:52]),
		MiniCutoff:         binary.LittleEndian.Uint32(data[56:60]),
		FirstMiniFATSector: binary.LittleEndian.Uint32(data[60:64]),
		NumMiniFATSectors:  binary.LittleEndian.Uint32(data[64:68]),
		FirstDIFATSector:   binary.LittleEndian.Uint32(data[68:72]),
		NumDIFATSectors:    binary.LittleEndian.Uint32(data[72:76]),
	}
	if h.SectorSize <= 0 || h.MiniSectorSize <= 0 {
		return h, fmt.Errorf("bad sector size")
	}
	for i := 76; i < 512; i += 4 {
		v := binary.LittleEndian.Uint32(data[i : i+4])
		if v != cfbFreeSect {
			h.DIFAT = append(h.DIFAT, v)
		}
	}
	curr := h.FirstDIFATSector
	for n := uint32(0); n < h.NumDIFATSectors && curr != cfbEndOfChain && curr != cfbFreeSect; n++ {
		sec, err := readCFBSector(data, h.SectorSize, curr)
		if err != nil {
			break
		}
		for i := 0; i+4 <= len(sec)-4; i += 4 {
			v := binary.LittleEndian.Uint32(sec[i : i+4])
			if v != cfbFreeSect {
				h.DIFAT = append(h.DIFAT, v)
			}
		}
		curr = binary.LittleEndian.Uint32(sec[len(sec)-4:])
	}
	return h, nil
}

func buildCFBFAT(data []byte, h cfbHeader) ([]uint32, error) {
	var fat []uint32
	for _, secID := range h.DIFAT {
		sec, err := readCFBSector(data, h.SectorSize, secID)
		if err != nil {
			continue
		}
		fat = append(fat, bytesToUint32Slice(sec)...)
	}
	if len(fat) == 0 {
		return nil, fmt.Errorf("empty fat")
	}
	return fat, nil
}

func readCFBSector(data []byte, sectorSize int, sectorID uint32) ([]byte, error) {
	off := int64(sectorID+1) * int64(sectorSize)
	if off < 0 || off+int64(sectorSize) > int64(len(data)) {
		return nil, fmt.Errorf("sector out of range")
	}
	return data[off : off+int64(sectorSize)], nil
}

func bytesToUint32Slice(b []byte) []uint32 {
	out := make([]uint32, 0, len(b)/4)
	for i := 0; i+4 <= len(b); i += 4 {
		out = append(out, binary.LittleEndian.Uint32(b[i:i+4]))
	}
	return out
}

func (d *cfbDocument) readSectorChain(start uint32, size int) ([]byte, error) {
	if start == cfbEndOfChain || start == cfbFreeSect {
		return nil, nil
	}
	var out bytes.Buffer
	seen := map[uint32]struct{}{}
	curr := start
	for curr != cfbEndOfChain && curr != cfbFreeSect {
		if _, ok := seen[curr]; ok {
			break
		}
		seen[curr] = struct{}{}
		sec, err := readCFBSector(d.Data, d.Header.SectorSize, curr)
		if err != nil {
			return nil, err
		}
		out.Write(sec)
		if int(curr) >= len(d.FAT) {
			break
		}
		curr = d.FAT[curr]
	}
	b := out.Bytes()
	if size > 0 && size < len(b) {
		b = b[:size]
	}
	return b, nil
}

func parseCFBDirectoryEntries(data []byte) []cfbDirEntry {
	entries := []cfbDirEntry{}
	for i := 0; i+128 <= len(data); i += 128 {
		chunk := data[i : i+128]
		nameLen := int(binary.LittleEndian.Uint16(chunk[64:66]))
		if nameLen < 2 || nameLen > 64 {
			continue
		}
		nameBytes := chunk[:nameLen-2]
		name := decodeUTF16LEBytes(nameBytes)
		entry := cfbDirEntry{
			Name:       name,
			ObjectType: chunk[66],
			Start:      binary.LittleEndian.Uint32(chunk[116:120]),
			Size:       binary.LittleEndian.Uint64(chunk[120:128]),
		}
		entries = append(entries, entry)
	}
	return entries
}

func decodeUTF16LEBytes(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return strings.TrimRight(string(utf16.Decode(u16)), "\x00")
}

func (d *cfbDocument) ReadStream(name string) ([]byte, error) {
	var entry *cfbDirEntry
	for i := range d.DirEntries {
		if d.DirEntries[i].ObjectType == 2 && d.DirEntries[i].Name == name {
			entry = &d.DirEntries[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("stream not found")
	}
	if entry.Size < uint64(d.Header.MiniCutoff) && len(d.MiniFAT) > 0 && len(d.MiniStream) > 0 {
		return d.readMiniStream(entry.Start, int(entry.Size))
	}
	return d.readSectorChain(entry.Start, int(entry.Size))
}

func (d *cfbDocument) readMiniStream(start uint32, size int) ([]byte, error) {
	if start == cfbEndOfChain || start == cfbFreeSect {
		return nil, nil
	}
	var out bytes.Buffer
	seen := map[uint32]struct{}{}
	curr := start
	for curr != cfbEndOfChain && curr != cfbFreeSect {
		if _, ok := seen[curr]; ok {
			break
		}
		seen[curr] = struct{}{}
		off := int(curr) * d.Header.MiniSectorSize
		end := off + d.Header.MiniSectorSize
		if off < 0 || end > len(d.MiniStream) {
			break
		}
		out.Write(d.MiniStream[off:end])
		if int(curr) >= len(d.MiniFAT) {
			break
		}
		curr = d.MiniFAT[curr]
	}
	b := out.Bytes()
	if size > 0 && size < len(b) {
		b = b[:size]
	}
	return b, nil
}

func parseOLEPropertySetStream(data []byte) map[string]string {
	out := map[string]string{}
	if len(data) < 48 {
		return out
	}
	numSets := binary.LittleEndian.Uint32(data[24:28])
	for i := uint32(0); i < numSets; i++ {
		base := 28 + int(i)*20
		if base+20 > len(data) {
			break
		}
		kind := oleSectionKind(data[base : base+16])
		offset := int(binary.LittleEndian.Uint32(data[base+16 : base+20]))
		for k, v := range parseOLEPropertySection(data, offset, kind) {
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	}
	return out
}

func oleSectionKind(fmtid []byte) string {
	if len(fmtid) != 16 {
		return ""
	}
	hexID := strings.ToUpper(hex.EncodeToString(fmtid))
	switch hexID {
	case "E0859FF2F94F6810AB9108002B27B3D9":
		return "summary"
	case "02D5CDD59C2E1B10939708002B2CF9AE":
		return "docsummary"
	case "05D5CDD59C2E1B10939708002B2CF9AE":
		return "userdefined"
	default:
		return ""
	}
}

func parseOLEPropertySection(data []byte, offset int, kind string) map[string]string {
	out := map[string]string{}
	if offset <= 0 || offset+8 > len(data) {
		return out
	}
	if offset+8 > len(data) {
		return out
	}
	propCount := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
	entriesBase := offset + 8
	for i := 0; i < propCount; i++ {
		entryOff := entriesBase + i*8
		if entryOff+8 > len(data) {
			break
		}
		propID := binary.LittleEndian.Uint32(data[entryOff : entryOff+4])
		valueOff := int(binary.LittleEndian.Uint32(data[entryOff+4 : entryOff+8]))
		name := olePropertyIDName(kind, propID)
		if name == "" {
			continue
		}
		value, ok := readOLETypedPropertyValue(data, offset+valueOff)
		if ok && strings.TrimSpace(value) != "" {
			out[name] = strings.TrimSpace(value)
		}
	}
	return out
}

func olePropertyIDName(kind string, id uint32) string {
	switch kind {
	case "summary":
		switch id {
		case 0x00000002:
			return "Title"
		case 0x00000003:
			return "Subject"
		case 0x00000004:
			return "Author"
		case 0x00000005:
			return "Keywords"
		case 0x00000006:
			return "Comments"
		case 0x00000007:
			return "Template"
		case 0x00000008:
			return "LastSavedBy"
		case 0x00000009:
			return "RevisionNumber"
		case 0x0000000C:
			return "CreateTime/Date"
		case 0x0000000D:
			return "LastSavedTime/Date"
		case 0x0000000E:
			return "PageCount"
		case 0x0000000F:
			return "WordCount"
		case 0x00000010:
			return "CharCount"
		case 0x00000012:
			return "ApplicationName"
		}
	case "docsummary":
		switch id {
		case 0x00000002:
			return "Category"
		case 0x0000000E:
			return "Manager"
		case 0x0000000F:
			return "Company"
		case 0x00000010:
			return "LinksDirty"
		case 0x00000007:
			return "SlideCount"
		case 0x00000004:
			return "PresentationTarget"
		}
	case "userdefined":
		return ""
	}
	return ""
}

func readOLETypedPropertyValue(data []byte, offset int) (string, bool) {
	if offset < 0 || offset+4 > len(data) {
		return "", false
	}
	vt := binary.LittleEndian.Uint16(data[offset : offset+2])
	switch vt {
	case 0x001E: // LPSTR
		if offset+8 > len(data) {
			return "", false
		}
		ln := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + ln
		if ln <= 0 || end > len(data) {
			return "", false
		}
		b := data[start:end]
		b = bytes.TrimRight(b, "\x00")
		return decodeOLEStringBytes(b), true
	case 0x001F: // LPWSTR
		if offset+8 > len(data) {
			return "", false
		}
		ln := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + ln*2
		if ln <= 0 || end > len(data) {
			return "", false
		}
		return decodeUTF16LEBytes(data[start:end]), true
	case 0x0040: // FILETIME
		if offset+12 > len(data) {
			return "", false
		}
		raw := binary.LittleEndian.Uint64(data[offset+4 : offset+12])
		if raw == 0 {
			return "", false
		}
		return filetimeToString(raw), true
	case 0x0003: // I4
		if offset+8 > len(data) {
			return "", false
		}
		return strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(data[offset+4:offset+8]))), 10), true
	case 0x000B: // BOOL
		if offset+8 > len(data) {
			return "", false
		}
		v := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		if v == 0 {
			return "false", true
		}
		return "true", true
	default:
		return "", false
	}
}

func decodeOLEStringBytes(b []byte) string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return ""
	}

	if looksLikeUTF16LEBytes(b) {
		return cleanLegacyMetadataString(decodeUTF16LEBytes(b))
	}
	if looksLikeUTF16BEBytes(b) {
		return cleanLegacyMetadataString(decodeUTF16BERaw(b))
	}

	utf8Candidate := cleanLegacyMetadataString(string(b))
	if utf8.Valid(b) && !looksLikeReplacementGarbage(utf8Candidate) {
		return utf8Candidate
	}

	cp1251Candidate := cleanLegacyMetadataString(decodeWindows1251(b))
	cp1252Candidate := cleanLegacyMetadataString(decodeWindows1252(b))

	utf8Score := scoreLegacyDecodedString(utf8Candidate)
	if scoreLegacyDecodedString(cp1251Candidate) > utf8Score {
		return cp1251Candidate
	}
	if scoreLegacyDecodedString(cp1252Candidate) > utf8Score {
		return cp1252Candidate
	}
	return utf8Candidate
}

func cleanLegacyMetadataString(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Map(func(r rune) rune {
		if r == 0 || (r < 32 && r != ' ') {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

func looksLikeReplacementGarbage(s string) bool {
	if s == "" {
		return false
	}
	return strings.ContainsRune(s, '�')
}

func scoreLegacyDecodedString(s string) int {
	if s == "" {
		return -9999
	}
	score := 0
	for _, r := range s {
		switch {
		case r == '�':
			score -= 5
		case r >= 'А' && r <= 'я':
			score += 3
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			score += 1
		case strings.ContainsRune(" .,_-@()[]{}+/\\&:#;", r):
			// neutral
		case r < 32:
			score -= 3
		default:
			// neutral
		}
	}
	return score
}

func looksLikeUTF16LEBytes(b []byte) bool {
	if len(b) < 4 || len(b)%2 != 0 {
		return false
	}
	zeros := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros >= len(b)/4
}

func looksLikeUTF16BEBytes(b []byte) bool {
	if len(b) < 4 || len(b)%2 != 0 {
		return false
	}
	zeros := 0
	for i := 0; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros >= len(b)/4
}

func decodeUTF16BERaw(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, binary.BigEndian.Uint16(b[i:i+2]))
	}
	return strings.TrimRight(string(utf16.Decode(u16)), "\x00")
}

var cp1251Map = map[byte]rune{
	0x80: 'Ђ', 0x81: 'Ѓ', 0x82: '‚', 0x83: 'ѓ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡',
	0x88: '€', 0x89: '‰', 0x8A: 'Љ', 0x8B: '‹', 0x8C: 'Њ', 0x8D: 'Ќ', 0x8E: 'Ћ', 0x8F: 'Џ',
	0x90: 'ђ', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x99: '™', 0x9A: 'љ', 0x9B: '›', 0x9C: 'њ', 0x9D: 'ќ', 0x9E: 'ћ', 0x9F: 'џ',
	0xA0: '\u00A0', 0xA1: 'Ў', 0xA2: 'ў', 0xA3: 'Ј', 0xA4: '¤', 0xA5: 'Ґ', 0xA6: '¦', 0xA7: '§',
	0xA9: '©', 0xAA: 'Є', 0xAB: '«', 0xAC: '¬', 0xAD: '\u00AD', 0xAE: '®', 0xAF: 'Ї',
	0xB0: '°', 0xB1: '±', 0xB2: 'І', 0xB3: 'і', 0xB4: 'ґ', 0xB5: 'µ', 0xB6: '¶', 0xB7: '·',
	0xB9: '№', 0xBA: 'є', 0xBB: '»', 0xBC: 'ј', 0xBD: 'Ѕ', 0xBE: 'ѕ', 0xBF: 'ї',
}

func decodeWindows1251(b []byte) string {
	runes := make([]rune, 0, len(b))
	for _, c := range b {
		switch {
		case c < 0x80:
			runes = append(runes, rune(c))
		case c >= 0xC0:
			runes = append(runes, rune(0x0410+int(c)-0xC0))
		case c == 0xA8:
			runes = append(runes, 'Ё')
		case c == 0xB8:
			runes = append(runes, 'ё')
		default:
			if r, ok := cp1251Map[c]; ok {
				runes = append(runes, r)
			} else {
				runes = append(runes, '�')
			}
		}
	}
	return string(runes)
}

var cp1252Map = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ',
	0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“',
	0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—', 0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›',
	0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

func decodeWindows1252(b []byte) string {
	runes := make([]rune, 0, len(b))
	for _, c := range b {
		if c < 0x80 || c >= 0xA0 {
			runes = append(runes, rune(c))
			continue
		}
		if r, ok := cp1252Map[c]; ok {
			runes = append(runes, r)
		} else {
			runes = append(runes, '�')
		}
	}
	return string(runes)
}

func filetimeToString(v uint64) string {
	const ticksPerSecond = 10000000
	const filetimeOffset = 11644473600
	sec := int64(v / ticksPerSecond)
	nsec := int64(v%ticksPerSecond) * 100
	sec -= filetimeOffset
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
}

func jobContext(job *Job) context.Context {
	return operationContext(job)
}

func operationContext(job *Job) context.Context {
	if job == nil {
		return context.Background()
	}
	job.mu.Lock()
	stopCh := job.stopCh
	stopped := job.stopRequested || job.Status.State == "canceled"
	job.mu.Unlock()
	if stopped {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		return c
	}
	if stopCh == nil {
		return context.Background()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

func isJobCanceled(job *Job) bool {
	if job == nil {
		return false
	}
	job.mu.Lock()
	stopped := job.stopRequested || job.Status.State == "canceled"
	stopCh := job.stopCh
	job.mu.Unlock()
	if stopped {
		return true
	}
	if stopCh == nil {
		return false
	}
	select {
	case <-stopCh:
		return true
	default:
		return false
	}
}

func isCancellationErr(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "context canceled"))
}

type documentsReportValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type documentsReportSection struct {
	Key    string                 `json:"key"`
	Total  int                    `json:"total"`
	Values []documentsReportValue `json:"values"`
}

type documentsReportData struct {
	Title            string                   `json:"title"`
	Language         string                   `json:"language"`
	Site             string                   `json:"site"`
	CreatedAt        string                   `json:"createdAt"`
	CreatedAtDisplay string                   `json:"createdAtDisplay"`
	Summary          documentsReportSummary   `json:"summary"`
	Categories       []documentsReportSection `json:"categories"`
}

type documentsReportSummary struct {
	DownloadedDocuments int `json:"downloadedDocuments"`
	WithMetadata        int `json:"withMetadata"`
	SectionsCount       int `json:"sectionsCount"`
	ValuesCount         int `json:"valuesCount"`
}

type reportEntry struct {
	Key   string
	Value string
}

func handleDocumentsReportPDF(w http.ResponseWriter, r *http.Request) {
	lang := normalizeReportLang(r.URL.Query().Get("lang"))
	jobID := strings.TrimSpace(r.URL.Query().Get("job"))
	if jobID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	v, ok := jobs.Load(jobID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)
	report := buildDocumentsReportExportData(job, lang)
	if report.Summary.DownloadedDocuments == 0 {
		http.Error(w, reportText(lang, "noDownloaded"), http.StatusBadRequest)
		return
	}
	pdfBytes, fileName, err := generateDocumentsReportPDF(report)
	if err != nil {
		http.Error(w, "pdf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pdfBytes)
}

func buildDocumentsReportExportData(job *Job, lang string) documentsReportData {
	now := time.Now()
	report := buildDocumentsReportData(job)
	site := ""
	if job != nil {
		job.mu.Lock()
		site = strings.TrimSpace(job.Params.StartURL)
		job.mu.Unlock()
	}
	return documentsReportData{
		Title:            reportText(lang, "title"),
		Language:         normalizeReportLang(lang),
		Site:             site,
		CreatedAt:        now.UTC().Format(time.RFC3339),
		CreatedAtDisplay: formatReportDate(now),
		Summary: documentsReportSummary{
			DownloadedDocuments: report.downloaded,
			WithMetadata:        report.withMetadata,
			SectionsCount:       len(report.sections),
			ValuesCount:         report.valuesCount,
		},
		Categories: localizedReportSections(report.sections, lang),
	}
}

func normalizeReportLang(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	switch lang {
	case "en":
		return "en"
	default:
		return "ru"
	}
}

func reportText(lang, key string) string {
	lang = normalizeReportLang(lang)
	values := map[string]map[string]string{
		"ru": {
			"title":        "Отчёт изучения метаданных",
			"site":         "Сайт",
			"createdAt":    "Дата создания",
			"downloaded":   "Скачано документов",
			"withMetadata": "С метаданными",
			"sections":     "Разделов метаданных",
			"values":       "Всего найденных значений",
			"noDownloaded": "Нет скачанных документов для экспорта.",
			"other":        "Другое",
		},
		"en": {
			"title":        "Metadata Analysis Report",
			"site":         "Site",
			"createdAt":    "Created at",
			"downloaded":   "Downloaded documents",
			"withMetadata": "With metadata",
			"sections":     "Metadata sections",
			"values":       "Total found values",
			"noDownloaded": "No downloaded documents to export.",
			"other":        "Other",
		},
	}
	if v, ok := values[lang][key]; ok {
		return v
	}
	return values["ru"][key]
}

func localizedReportSections(sections []documentsReportSection, lang string) []documentsReportSection {
	out := make([]documentsReportSection, 0, len(sections))
	for _, section := range sections {
		if strings.EqualFold(strings.TrimSpace(section.Key), "Другое") {
			section.Key = reportText(lang, "other")
		}
		out = append(out, section)
	}
	return out
}

type builtDocumentsReport struct {
	downloaded   int
	withMetadata int
	valuesCount  int
	sections     []documentsReportSection
}

func buildDocumentsReportData(job *Job) builtDocumentsReport {
	if job == nil {
		return builtDocumentsReport{}
	}
	job.mu.Lock()
	docs := make([]DocumentItem, len(job.Documents))
	copy(docs, job.Documents)
	job.mu.Unlock()

	downloadedDocs := make([]DocumentItem, 0)
	for _, doc := range docs {
		if doc.Downloaded {
			downloadedDocs = append(downloadedDocs, doc)
		}
	}
	groups := map[string]map[string]int{}
	sectionTotals := map[string]int{}
	withMeta := 0
	valuesCount := 0
	for _, doc := range downloadedDocs {
		entries := collectDocMetadataEntriesForReport(doc)
		if len(entries) > 0 {
			withMeta++
		}
		for _, entry := range entries {
			if strings.TrimSpace(entry.Key) == "" || strings.TrimSpace(entry.Value) == "" {
				continue
			}
			if groups[entry.Key] == nil {
				groups[entry.Key] = map[string]int{}
			}
			groups[entry.Key][entry.Value]++
			sectionTotals[entry.Key]++
			valuesCount++
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	sections := make([]documentsReportSection, 0, len(keys))
	for _, key := range keys {
		vals := make([]documentsReportValue, 0, len(groups[key]))
		for value, count := range groups[key] {
			vals = append(vals, documentsReportValue{Value: value, Count: count})
		}
		sort.Slice(vals, func(i, j int) bool {
			if vals[i].Count != vals[j].Count {
				return vals[i].Count > vals[j].Count
			}
			return strings.ToLower(vals[i].Value) < strings.ToLower(vals[j].Value)
		})
		sections = append(sections, documentsReportSection{Key: key, Total: sectionTotals[key], Values: vals})
	}
	return builtDocumentsReport{downloaded: len(downloadedDocs), withMetadata: withMeta, valuesCount: valuesCount, sections: sections}
}

func collectDocMetadataEntriesForReport(doc DocumentItem) []reportEntry {
	entries := make([]reportEntry, 0)
	add := func(key, value string) {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return
		}
		entries = append(entries, reportEntry{Key: key, Value: value})
	}
	pairs := [][2]string{{"Title", doc.Title}, {"Author", doc.Author}, {"Company", doc.Company}, {"Creator", doc.Creator}, {"Producer", doc.Producer}, {"LastModifiedBy", doc.LastModifiedBy}, {"Created", doc.Created}, {"Modified", doc.Modified}}
	for _, p := range pairs {
		if p[1] != "" {
			add(p[0], p[1])
		}
	}
	if strings.TrimSpace(doc.Contacts) != "" {
		for _, v := range strings.Split(doc.Contacts, ",") {
			v = strings.TrimSpace(v)
			if v != "" {
				add("Contacts", v)
			}
		}
	}
	known := map[string]struct{}{}
	for _, item := range entries {
		known[strings.ToLower(item.Key+"\x00"+item.Value)] = struct{}{}
	}
	parts := splitMetadataPartsForReport(firstNonEmpty(doc.MetadataRawSummary, doc.MetadataShort))
	knownKeys := map[string]struct{}{"title": {}, "author": {}, "company": {}, "creator": {}, "producer": {}, "lastmodifiedby": {}, "created": {}, "modified": {}, "contacts": {}, "app": {}}
	for _, part := range parts {
		idx := strings.Index(part, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimLeft(strings.TrimSpace(part[:idx]), "|")
		key = strings.TrimSpace(key)
		value := strings.TrimSpace(part[idx+1:])
		if key == "" || value == "" {
			continue
		}
		if strings.EqualFold(key, "count") {
			continue
		}
		storeKey := key
		if strings.EqualFold(storeKey, "app") {
			storeKey = "App"
		}
		if _, ok := knownKeys[strings.ToLower(storeKey)]; !ok {
			value = key + ": " + value
			storeKey = "Другое"
		}
		sig := strings.ToLower(storeKey + "\x00" + value)
		if _, ok := known[sig]; ok {
			continue
		}
		known[sig] = struct{}{}
		add(storeKey, value)
	}
	return entries
}

func splitMetadataPartsForReport(text string) []string {
	parts := strings.Split(text, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.TrimLeft(part, "|"))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func formatReportDate(t time.Time) string {
	return t.Format("02.01.2006 15:04")
}

func sanitizeExportFilePart(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return "report"
	}
	v = regexp.MustCompile(`^https?://`).ReplaceAllString(v, "")
	v = regexp.MustCompile(`[^a-zA-Z0-9а-яА-ЯёЁ._-]+`).ReplaceAllString(v, "_")
	v = strings.Trim(v, "_")
	if len([]rune(v)) > 60 {
		v = string([]rune(v)[:60])
	}
	if v == "" {
		return "report"
	}
	return v
}

func generateDocumentsReportPDF(report documentsReportData) ([]byte, string, error) {
	fontPath, err := findPDFReportFontPath()
	if err != nil {
		return nil, "", err
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 15)
	pdf.AliasNbPages("")
	pdf.AddUTF8Font("main", "", fontPath)
	pdf.SetFont("main", "", 11)
	pdf.AddPage()

	pdf.SetFont("main", "", 18)
	pdf.CellFormat(0, 10, report.Title, "", 1, "L", false, 0, "")
	pdf.Ln(2)
	pdf.SetFont("main", "", 11)
	lang := normalizeReportLang(report.Language)
	pdf.MultiCell(0, 6, reportText(lang, "site")+": "+firstNonEmpty(report.Site, "—"), "", "L", false)
	pdf.MultiCell(0, 6, reportText(lang, "createdAt")+": "+report.CreatedAtDisplay, "", "L", false)
	pdf.Ln(3)

	cards := [][2]string{{reportText(lang, "downloaded"), strconv.Itoa(report.Summary.DownloadedDocuments)}, {reportText(lang, "withMetadata"), strconv.Itoa(report.Summary.WithMetadata)}, {reportText(lang, "sections"), strconv.Itoa(report.Summary.SectionsCount)}, {reportText(lang, "values"), strconv.Itoa(report.Summary.ValuesCount)}}
	pdf.SetFont("main", "", 12)
	maxLabelW := 0.0
	for _, card := range cards {
		w := pdf.GetStringWidth(card[0]+":") + 4
		if w > maxLabelW {
			maxLabelW = w
		}
	}
	if maxLabelW < 40 {
		maxLabelW = 40
	}
	if maxLabelW > 110 {
		maxLabelW = 110
	}
	leftMargin, _, _, _ := pdf.GetMargins()
	for _, card := range cards {
		pdf.SetX(leftMargin)
		pdf.SetFont("main", "", 12)
		pdf.CellFormat(maxLabelW, 8, card[0]+":", "", 0, "L", false, 0, "")
		pdf.CellFormat(0, 8, card[1], "", 1, "L", false, 0, "")
	}
	pdf.Ln(2)

	for _, section := range report.Categories {
		pdf.SetFont("main", "", 14)
		pdf.SetFillColor(236, 240, 247)
		pdf.CellFormat(0, 9, fmt.Sprintf("%s (%d)", section.Key, section.Total), "", 1, "L", true, 0, "")
		pdf.Ln(1)
		for _, item := range section.Values {
			line := fmt.Sprintf("- %s (%d)", item.Value, item.Count)
			pdf.SetFont("main", "", 11)
			pdf.MultiCell(0, 6, line, "", "L", false)
		}
		pdf.Ln(3)
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, "", err
	}
	fileName := "metadata-report-" + sanitizeExportFilePart(report.Site) + ".pdf"
	return buf.Bytes(), fileName, nil
}

func findPDFReportFontPath() (string, error) {
	if path, err := ensureBundledPDFReportFontPath(); err == nil && path != "" {
		return path, nil
	}

	candidates := make([]string, 0)
	if runtime.GOOS == "windows" {
		winDir := os.Getenv("WINDIR")
		if winDir == "" {
			winDir = `C:\Windows`
		}
		candidates = append(candidates,
			filepath.Join(winDir, "Fonts", "arial.ttf"),
			filepath.Join(winDir, "Fonts", "calibri.ttf"),
			filepath.Join(winDir, "Fonts", "segoeui.ttf"),
		)
	} else if runtime.GOOS == "darwin" {
		candidates = append(candidates,
			"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
			"/System/Library/Fonts/Supplemental/Arial.ttf",
			"/Library/Fonts/Arial.ttf",
		)
	} else {
		candidates = append(candidates,
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			"/usr/share/fonts/truetype/liberation2/LiberationSans-Regular.ttf",
			"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
		)
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("не найден TTF-шрифт для PDF: встроенный шрифт недоступен и системные шрифты не найдены")
}

func ensureBundledPDFReportFontPath() (string, error) {
	if len(bundledPDFReportFont) == 0 {
		return "", fmt.Errorf("встроенный TTF-шрифт пуст")
	}
	fontPath := filepath.Join(os.TempDir(), "links-overseer-DejaVuSans.ttf")
	if st, err := os.Stat(fontPath); err == nil && !st.IsDir() && st.Size() == int64(len(bundledPDFReportFont)) {
		return fontPath, nil
	}
	if err := os.WriteFile(fontPath, bundledPDFReportFont, 0o644); err != nil {
		return "", err
	}
	return fontPath, nil
}
