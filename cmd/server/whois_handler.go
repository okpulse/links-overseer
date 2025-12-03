package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/okpulse/links-overseer/internal/core"
)

// handleWhois обрабатывает запросы WHOIS и не зависит от состояния сканирования.
// Ожидает GET /api/whois?target=<url_or_domain>
func handleWhois(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	info, err := core.FetchWhois(ctx, target)
	if err != nil {
		http.Error(w, "whois error: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(info)
}
