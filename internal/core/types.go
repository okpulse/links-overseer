package core

type Scope int

const (
	ScopeAll Scope = iota
	ScopeInternalOnly
	ScopeExternalOnly
)

type StatusClass int

const (
	StatusAny StatusClass = iota
	Status2xx
	Status3xx
	Status4xx
	Status5xx
	StatusError
)

type Result struct {
	URL        string `json:"url"`
	PageURL    string `json:"page_url"`
	Internal   bool   `json:"internal"`
	StatusCode int    `json:"status_code"`
	Err        string `json:"error,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	Depth      int    `json:"depth"`
}

type ImageRef struct {
	URL     string
	PageURL string
	Alt     string
}

type DocumentRef struct {
	URL      string
	PageURL  string
	FileName string
	FileType string
}

type JobParams struct {
	StartURL          string `json:"start_url"`
	MaxDepth          int    `json:"max_depth"`
	RespectRobots     bool   `json:"respect_robots"`
	DownloadImages    bool   `json:"download_images"`
	DownloadDocuments bool   `json:"download_documents"`
}

type JobStatus struct {
	State        string `json:"state"`
	Visited      int    `json:"visited"`
	Queued       int    `json:"queued"`
	Discovered   int    `json:"discovered"`
	Errors       int    `json:"errors"`
	CheckedLinks int    `json:"checked_links"`
	TotalLinks   int    `json:"total_links"`
}

type Filter struct {
	Scope  Scope         `json:"scope"`
	Status []StatusClass `json:"status"`
}
