package core

import (
	"net/url"
	"path"
	"strings"
)

func DetectDocumentType(p string) (string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	switch ext {
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt":
		return ext, true
	default:
		return "", false
	}
}

func GuessFileNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" {
		return ""
	}
	return name
}
