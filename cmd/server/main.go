package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/okpulse/links-overseer/internal/core"
)

//go:embed web/static/*
var staticFS embed.FS

type Job struct {
	ID      string
	Params  core.JobParams
	Status  core.JobStatus
	Results []core.Result

	mu      sync.Mutex
	cancel  context.CancelFunc
}

var jobs sync.Map

func newID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b { b[i] = letters[rand.Intn(len(letters))] }
	return string(b)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin": cmd = exec.Command("open", url)
	case "windows": cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	rand.Seed(time.Now().UnixNano())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { log.Fatal(err) }
	addr := ln.Addr().String()
	ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/app.js", handleAppJS)
	mux.HandleFunc("/styles.css", handleCSS)
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/results", handleResults)
	mux.HandleFunc("/api/stop", handleStop)

	srv := &http.Server{ Addr: addr, Handler: mux, ReadTimeout: 10*time.Second, WriteTimeout: 300*time.Second }
	url := "http://" + addr + "/"
	fmt.Println("Serving at", url)
	go func(){ time.Sleep(300*time.Millisecond); openBrowser(url) }()

	log.Fatal(srv.ListenAndServe())
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	b, _ := staticFS.ReadFile("web/static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}
func handleAppJS(w http.ResponseWriter, r *http.Request) {
	b, _ := staticFS.ReadFile("web/static/app.js")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Write(b)
}
func handleCSS(w http.ResponseWriter, r *http.Request) {
	b, _ := staticFS.ReadFile("web/static/styles.css")
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Write(b)
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method", http.StatusMethodNotAllowed); return }
	type Req struct {
		StartURL string `json:"start_url"`
		Depth    int    `json:"depth"`
		RespectRobots bool `json:"respect_robots"`
	}
	var req Req
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StartURL == "" {
		http.Error(w, "bad request", http.StatusBadRequest); return
	}
	if req.Depth < 0 { req.Depth = 0 }
	if req.Depth == 0 { /* only start page */ }
	if req.Depth > 5 { req.Depth = 5 }

	u, err := url.Parse(req.StartURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest); return
	}

	job := &Job{
		ID: newID(),
		Params: core.JobParams{ StartURL: u.String(), MaxDepth: req.Depth, RespectRobots: req.RespectRobots },
		Status: core.JobStatus{ State: "queued" },
		Results: make([]core.Result, 0),
	}
	jobs.Store(job.ID, job)

	go runJob(job)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"job_id": job.ID})
}

func runJob(job *Job) {
	job.mu.Lock(); job.Status.State = "running"; job.mu.Unlock()

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

	progress := func(p core.CrawlProgress) {
		job.mu.Lock()
		if p.Visited != 0 { job.Status.Visited = p.Visited }
		if p.Queued != 0 { job.Status.Queued = p.Queued }
		if p.Discovered != 0 { job.Status.Discovered = p.Discovered }
		job.Status.Errors = p.Errors
		if p.CheckedLinks != 0 || job.Status.CheckedLinks == 0 { job.Status.CheckedLinks = p.CheckedLinks }
		if p.TotalLinks != 0 || job.Status.TotalLinks == 0 { job.Status.TotalLinks = p.TotalLinks }
		job.mu.Unlock()
	}
	if err := crawler.Crawl(ctx, u, progress, sink); err != nil {
		job.mu.Lock(); job.Status.State = "failed"; job.mu.Unlock()
		return
	}

	job.mu.Lock(); job.Status.State = "done"; job.mu.Unlock()
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok { http.Error(w, "not found", http.StatusNotFound); return }
	job := v.(*Job)
	job.mu.Lock(); st := job.Status; job.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func handleResults(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok { http.Error(w, "not found", http.StatusNotFound); return }
	job := v.(*Job)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job.Results)
}

func parseScope(s string) core.Scope {
	s = strings.ToLower(s)
	switch s {
	case "internal": return core.ScopeInternalOnly
	case "external": return core.ScopeExternalOnly
	default: return core.ScopeAll
	}
}

func parseStatuses(q string) []core.StatusClass {
	if q == "" { return nil }
	parts := strings.Split(q, ",")
	sts := []core.StatusClass{}
	for _, p := range parts {
		s := strings.TrimSpace(strings.ToLower(p))
		switch s {
		case "2": sts = append(sts, core.Status2xx)
		case "3": sts = append(sts, core.Status3xx)
		case "4": sts = append(sts, core.Status4xx)
		case "5": sts = append(sts, core.Status5xx)
		case "e": sts = append(sts, core.StatusError)
		}
	}
	slices.SortFunc(sts, func(a,b core.StatusClass) int { return int(a)-int(b) })
	sts = slices.Compact(sts)
	return sts
}


func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method", http.StatusMethodNotAllowed); return }
	id := r.URL.Query().Get("job")
	v, ok := jobs.Load(id)
	if !ok { http.Error(w, "not found", http.StatusNotFound); return }
	job := v.(*Job)
	if job.cancel != nil {
		job.cancel()
		job.mu.Lock(); job.Status.State = "canceled"; job.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}
