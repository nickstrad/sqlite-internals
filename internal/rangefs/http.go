package rangefs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

type HTTPSource struct {
	BaseURL string
	Client  *http.Client
}

func (h HTTPSource) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

func (h HTTPSource) url(name string) string {
	return strings.TrimSuffix(h.BaseURL, "/") + "/" + (&url.URL{Path: name}).EscapedPath()
}

func (h HTTPSource) Stat(ctx context.Context, name string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, h.url(name), nil)
	if err != nil {
		return 0, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, fs.ErrNotExist
	default:
		return 0, fmt.Errorf("HEAD %s: %s", name, resp.Status)
	}
	if resp.ContentLength < 0 {
		return 0, fmt.Errorf("HEAD %s: no Content-Length", name)
	}
	return resp.ContentLength, nil
}

func (h HTTPSource) ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(name), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1))
	resp, err := h.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("GET %s range %d+%d: %s (want 206 Partial Content)", name, offset, len(p), resp.Status)
	}
	return io.ReadFull(resp.Body, p)
}
