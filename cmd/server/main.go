package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okpulse/links-overseer/internal/core"
	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/net/html"
)

//go:embed web/static/*
var staticFS embed.FS

const (
	maxImagesPerJob   = 2000
	maxImageSizeBytes = 10 * 1024 * 1024

	maxPagesForImages = 100 // ограничение количества страниц, с которых собираем картинки
)

type Job struct {
	ID      string
	Params  core.JobParams
	Status  core.JobStatus
	Results []core.Result

	Images    []ImageItem
	nextImgID int64

	mu     sync.Mutex
	cancel context.CancelFunc
}

type ImageItem struct {
	ID            int64  `json:"id"`
	ImageURL      string `json:"imageUrl"`
	PageURL       string `json:"pageUrl"`
	Alt           string `json:"alt"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	MetadataShort string `json:"metadataShort"`
	HasMetadata   bool   `json:"hasMetadata"`
	Downloaded    bool   `json:"downloaded"`
	TooLarge      bool   `json:"tooLarge"`
	PreviewURL    string `json:"previewUrl"`
}

var jobs sync.Map

func ensureDataDir(job *Job) {
	u, err := url.Parse(job.Params.StartURL)
	if err != nil {
		return
	}
	host := u.Hostname()
	if host == "" {
		host = "site"
	}
	dir := filepath.Join("data", host)
	_ = os.MkdirAll(dir, 0o755)
}

func newID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	mux := http.NewServeMux()

	// Статические файлы из embed (index.html, app.js, styles.css)
	sub, err := fs.Sub(staticFS, "web/static")
	if err != nil {
		log.Fatalf("static FS: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// API
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/results", handleResults)
	mux.HandleFunc("/api/images", handleImages)
	mux.HandleFunc("/api/images/download", handleImageDownload)
	mux.HandleFunc("/api/stop", handleStop)

	// скачанные картинки: ./data/<host>/...
	mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir("data"))))

	addr := ":8080"
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("Listening on %s", addr)
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser("http://localhost" + addr + "/")
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StartURL       string `json:"start_url"`
		Depth          int    `json:"depth"`
		RespectRobots  bool   `json:"respect_robots"`
		DownloadImages bool   `json:"download_images"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	if req.Depth < 0 {
		req.Depth = 0
	}
	if req.Depth > 5 {
		req.Depth = 5
	}

	u, err := url.Parse(req.StartURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	job := &Job{
		ID: newID(),
		Params: core.JobParams{
			StartURL:       u.String(),
			MaxDepth:       req.Depth,
			RespectRobots:  req.RespectRobots,
			DownloadImages: req.DownloadImages,
		},
		Status: core.JobStatus{
			State: "queued",
		},
		Results: make([]core.Result, 0),
		Images:  make([]ImageItem, 0),
	}

	jobs.Store(job.ID, job)
	go runJob(job)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job_id": job.ID})
}

func runJob(job *Job) {
	job.mu.Lock()
	job.Status.State = "running"
	job.mu.Unlock()

	if job.Params.DownloadImages {
		ensureDataDir(job)
	}

	u, _ := url.Parse(job.Params.StartURL)
	checker := core.NewChecker("PulseLinkChecker/1.0 (+local)")
	crawler := core.NewCrawler(u, job.Params.MaxDepth, job.Params.RespectRobots, checker)

	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel

	resMu := sync.Mutex{}
	sink := func(r core.Result) {
		resMu.Lock()
		job.Results = append(job.Results, r)
		resMu.Unlock()
	}

	imgSink := func(ir core.ImageRef) {
		addImageToJob(job, ir.URL, ir.PageURL, ir.Alt)
	}

	progress := func(p core.CrawlProgress) {
		job.mu.Lock()
		if p.Visited != 0 {
			job.Status.Visited = p.Visited
		}
		if p.Queued != 0 {
			job.Status.Queued = p.Queued
		}
		if p.Discovered != 0 {
			job.Status.Discovered = p.Discovered
		}
		job.Status.Errors = p.Errors
		if p.CheckedLinks != 0 || job.Status.CheckedLinks == 0 {
			job.Status.CheckedLinks = p.CheckedLinks
		}
		if p.TotalLinks != 0 || job.Status.TotalLinks == 0 {
			job.Status.TotalLinks = p.TotalLinks
		}
		job.mu.Unlock()
	}

	if err := crawler.Crawl(ctx, u, progress, sink, imgSink); err != nil {
		job.mu.Lock()
		job.Status.State = "failed"
		job.mu.Unlock()
		return
	}

	// Дополнительный проход
	collectImagesForJob(job)

	job.mu.Lock()
	job.Status.State = "done"
	job.mu.Unlock()
}

func addImageToJob(job *Job, imgURL, pageURL, alt string) {
	if imgURL == "" {
		return
	}

	job.mu.Lock()

	// Проверка на дубликаты
	for _, ex := range job.Images {
		if ex.ImageURL == imgURL && ex.PageURL == pageURL {
			job.mu.Unlock()
			return
		}
	}

	if len(job.Images) >= maxImagesPerJob {
		job.mu.Unlock()
		return
	}

	job.nextImgID++
	id := job.nextImgID
	item := ImageItem{
		ID:         id,
		ImageURL:   imgURL,
		PageURL:    pageURL,
		Alt:        alt,
		PreviewURL: imgURL,
	}
	job.Images = append(job.Images, item)
	job.mu.Unlock()

	// скачать и вытащить метаданные.
	if job.Params.DownloadImages {
		if _, err := downloadImageForJob(job, id); err != nil {
			log.Printf("image download error for job %s id %d: %v", job.ID, id, err)
		}
	}
}

func collectImagesForJob(job *Job) {
	job.mu.Lock()
	startURL := job.Params.StartURL
	results := make([]core.Result, len(job.Results))
	copy(results, job.Results)
	job.mu.Unlock()

	pageSet := map[string]struct{}{}
	if startURL != "" {
		pageSet[startURL] = struct{}{}
	}
	for _, r := range results {
		if r.PageURL != "" {
			pageSet[r.PageURL] = struct{}{}
		}
	}

	client := &http.Client{Timeout: 20 * time.Second}

	count := 0
	for pageURL := range pageSet {
		if count >= maxPagesForImages {
			break
		}

		u, err := url.Parse(pageURL)
		if err != nil {
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}

		resp, err := client.Get(pageURL)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		ct := resp.Header.Get("Content-Type")
		if ct != "" && !strings.HasPrefix(ct, "text/html") {
			resp.Body.Close()
			continue
		}

		doc, err := html.Parse(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		// Обход DOM и сбор <img>.
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && strings.EqualFold(n.Data, "img") {
				var alt string
				candidates := []string{}

				for _, a := range n.Attr {
					key := strings.ToLower(a.Key)
					val := strings.TrimSpace(a.Val)

					if key == "alt" {
						alt = a.Val
					}
					if key == "src" && val != "" {
						candidates = append(candidates, val)
					}
					if (key == "data-src" || key == "data-lazy-src" || key == "data-original") && val != "" {
						candidates = append(candidates, val)
					}
					if (key == "srcset" || key == "data-srcset") && val != "" {
						parts := strings.Split(val, ",")
						if len(parts) > 0 {
							first := strings.TrimSpace(parts[0])
							fields := strings.Fields(first)
							if len(fields) > 0 {
								candidates = append(candidates, fields[0])
							}
						}
					}
				}

				for _, raw := range candidates {
					if raw == "" {
						continue
					}
					u2, err := u.Parse(raw)
					if err != nil {
						continue
					}
					if u2.Scheme == "data" {
						continue
					}
					if u2.Scheme == "" || u2.Scheme == "http" || u2.Scheme == "https" {
						addImageToJob(job, u2.String(), pageURL, alt)
						break
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)

		count++
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)

	job.mu.Lock()
	st := job.Status
	job.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func handleResults(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)

	job.mu.Lock()
	res := make([]core.Result, len(job.Results))
	copy(res, job.Results)
	job.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func handleImages(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)

	job.mu.Lock()
	imgs := make([]ImageItem, len(job.Images))
	copy(imgs, job.Images)
	job.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(imgs)
}

func handleImageDownload(w http.ResponseWriter, r *http.Request) {
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

	item, err := downloadImageForJob(job, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(item)
}

func downloadImageForJob(job *Job, id int64) (*ImageItem, error) {
	job.mu.Lock()
	startURL := job.Params.StartURL
	idx := -1
	for i := range job.Images {
		if job.Images[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		job.mu.Unlock()
		return nil, fmt.Errorf("image not found")
	}

	if job.Images[idx].TooLarge {
		itemCopy := job.Images[idx]
		job.mu.Unlock()
		return &itemCopy, nil
	}

	imgURL := job.Images[idx].ImageURL
	job.mu.Unlock()

	u, err := url.Parse(startURL)
	if err != nil {
		return nil, fmt.Errorf("bad start url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		host = "site"
	}

	dir := filepath.Join("data", host)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(imgURL)
	if err != nil {
		return nil, fmt.Errorf("get image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	n, err := io.CopyN(&buf, resp.Body, maxImageSizeBytes+1)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read: %w", err)
	}
	if n > maxImageSizeBytes {
		job.mu.Lock()
		job.Images[idx].TooLarge = true
		itemCopy := job.Images[idx]
		job.mu.Unlock()
		return &itemCopy, nil
	}

	data := buf.Bytes()

	width, height := 0, 0
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		width = cfg.Width
		height = cfg.Height
	}

	hasMeta := false
	metaShort := ""
	if ex, err := exif.Decode(bytes.NewReader(data)); err == nil {
		_ = ex
		hasMeta = true
		metaShort = "EXIF найдено"
	}

	imgU, err := url.Parse(imgURL)
	if err != nil {
		return nil, fmt.Errorf("bad image url: %w", err)
	}
	name := filepath.Base(imgU.Path)
	if name == "" || name == "." || name == "/" {
		name = fmt.Sprintf("image_%d", id)
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	preview := "/images/" + host + "/" + name

	job.mu.Lock()
	job.Images[idx].Width = width
	job.Images[idx].Height = height
	job.Images[idx].HasMetadata = hasMeta
	job.Images[idx].MetadataShort = metaShort
	job.Images[idx].Downloaded = true
	job.Images[idx].TooLarge = false
	job.Images[idx].PreviewURL = preview
	itemCopy := job.Images[idx]
	job.mu.Unlock()

	return &itemCopy, nil
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	job := v.(*Job)
	if job.cancel != nil {
		job.cancel()
		job.mu.Lock()
		job.Status.State = "canceled"
		job.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}
