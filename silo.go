package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errRateLimited is returned when Silo still answers 429 after retries.
var errRateLimited = errors.New("rate limited by Silo")

// errBadRequest is returned when Silo answers 400.
var errBadRequest = errors.New("rejected by Silo")

// errNotFound is returned when Silo answers 404 (e.g. a collection someone
// deleted by hand in Silo).
var errNotFound = errors.New("not found in Silo")

// SiloClient calls Silo's own admin REST API with an admin API key. The plugin
// SDK has no host call for writing artwork or collections, so this is the only
// supported write path.
//
// Silo rate-limits each API key (20 requests/second by default), so every call
// goes through a pacer that stays under that, and a 429 is retried after the
// delay Silo asks for.
type SiloClient struct {
	baseURL string // e.g. http://127.0.0.1:8080
	apiKey  string
	http    *http.Client
	pace    *pacer
}

const (
	siloRequestsPerSecond = 15
	maxRateLimitRetries   = 6
)

func newSiloClient(baseURL, apiKey string) *SiloClient {
	return &SiloClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Minute},
		pace:    newPacer(siloRequestsPerSecond),
	}
}

// pacer spaces calls evenly at a fixed rate across all goroutines.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newPacer(perSecond int) *pacer {
	return &pacer{interval: time.Second / time.Duration(perSecond)}
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	slot := p.next
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	return sleepCtx(ctx, time.Until(slot))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAfter reads how long Silo asked us to wait on a 429.
func retryAfter(resp *http.Response, body []byte) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	var parsed struct {
		RetryAfter int `json:"retry_after"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.RetryAfter > 0 {
		return time.Duration(parsed.RetryAfter) * time.Second
	}
	return time.Second
}

func (s *SiloClient) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	for attempt := 0; ; attempt++ {
		if err := s.pace.wait(ctx); err != nil {
			return err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, s.baseURL+"/api/v1"+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := s.http.Do(req)
		if err != nil {
			return fmt.Errorf("silo %s %s: %w", method, path, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if err := sleepCtx(ctx, retryAfter(resp, snippet)); err != nil {
				return err
			}
			continue
		}
		err = s.handle(resp, method, path, out)
		resp.Body.Close()
		return err
	}
}

func (s *SiloClient) handle(resp *http.Response, method, path string, out any) error {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("silo %s %s: %w", method, path, errNotFound)
	case resp.StatusCode == http.StatusBadRequest:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("silo %s %s: %w: %s", method, path, errBadRequest, strings.TrimSpace(string(snippet)))
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("silo %s %s: %w", method, path, errRateLimited)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("silo %s %s: HTTP %d (the API key must be an unscoped admin key)", method, path, resp.StatusCode)
	case resp.StatusCode >= 300:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("silo %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (s *SiloClient) doJSON(ctx context.Context, method, path string, payload, out any) error {
	var body []byte
	contentType := ""
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = data
		contentType = "application/json"
	}
	return s.do(ctx, method, path, contentType, body, out)
}

// ApplyPoster tells Silo to download imageURL and make it the item's poster.
// Silo locks the item's image field afterwards, so a later metadata refresh
// does not replace it. contentID must be a movie or series; season and episode
// IDs are never passed here.
func (s *SiloClient) ApplyPoster(ctx context.Context, contentID, imageURL string) error {
	return s.doJSON(ctx, http.MethodPost, "/admin/items/"+url.PathEscape(contentID)+"/images/apply",
		map[string]string{"original_url": imageURL, "type": "poster"}, nil)
}

type siloCollection struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// siloCollectionInfo is one entry of Silo's admin collection list.
type siloCollectionInfo struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	LibraryIDs []int  `json:"library_ids"`
}

// ListCollections returns every server collection, across all libraries.
func (s *SiloClient) ListCollections(ctx context.Context) ([]siloCollectionInfo, error) {
	var out struct {
		Collections []siloCollectionInfo `json:"collections"`
	}
	err := s.doJSON(ctx, http.MethodGet, "/admin/collections", nil, &out)
	return out.Collections, err
}

// CreateCollection creates a manual (hand-curated) server collection.
func (s *SiloClient) CreateCollection(ctx context.Context, title, slug string, libraryIDs []int) (string, error) {
	var out siloCollection
	err := s.doJSON(ctx, http.MethodPost, "/admin/collections", map[string]any{
		"title":           title,
		"slug":            slug,
		"library_ids":     libraryIDs,
		"collection_type": "manual",
		"description":     "Synced from Plex by Kometa Poster Sync.",
	}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("silo create collection %q: response had no id", title)
	}
	return out.ID, nil
}

// UpdateCollection updates title/libraries and, when poster is non-nil,
// uploads a new poster in the same request.
func (s *SiloClient) UpdateCollection(ctx context.Context, id, title string, libraryIDs []int, poster []byte) error {
	data, err := json.Marshal(map[string]any{"title": title, "library_ids": libraryIDs})
	if err != nil {
		return err
	}
	path := "/admin/collections/" + url.PathEscape(id)
	if poster == nil {
		return s.do(ctx, http.MethodPut, path, "application/json", data, nil)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("data", string(data)); err != nil {
		return err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="poster"; filename="poster"`)
	header.Set("Content-Type", sniffImageType(poster))
	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(poster); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return s.do(ctx, http.MethodPut, path, w.FormDataContentType(), buf.Bytes(), nil)
}

func (s *SiloClient) AddCollectionItem(ctx context.Context, collectionID, contentID string, position int) error {
	return s.doJSON(ctx, http.MethodPut,
		"/admin/collections/"+url.PathEscape(collectionID)+"/items/"+url.PathEscape(contentID),
		map[string]int{"position": position}, nil)
}

// ReorderCollection sets the order of every item in a collection.
func (s *SiloClient) ReorderCollection(ctx context.Context, collectionID string, ordered []string) error {
	return s.doJSON(ctx, http.MethodPut, "/admin/collections/"+url.PathEscape(collectionID)+"/items/order",
		map[string][]string{"ordered_ids": ordered}, nil)
}

func (s *SiloClient) RemoveCollectionItem(ctx context.Context, collectionID, contentID string) error {
	err := s.doJSON(ctx, http.MethodDelete,
		"/admin/collections/"+url.PathEscape(collectionID)+"/items/"+url.PathEscape(contentID), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

func (s *SiloClient) DeleteCollection(ctx context.Context, collectionID string) error {
	err := s.doJSON(ctx, http.MethodDelete, "/admin/collections/"+url.PathEscape(collectionID), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// fieldImagesLock is Silo's FieldImages lock (metadata.FieldImages). Silo sets
// it when a poster is applied, which is what stops a refresh replacing it.
const fieldImagesLock = 10

// LockedFields returns the item's locked metadata fields.
func (s *SiloClient) LockedFields(ctx context.Context, contentID string) ([]int, error) {
	var out struct {
		LockedFields []int `json:"locked_fields"`
	}
	err := s.doJSON(ctx, http.MethodGet, "/catalog/items/"+url.PathEscape(contentID), nil, &out)
	return out.LockedFields, err
}

// SetLockedFields replaces the item's locked fields (Silo stores the list
// whole, so callers pass every lock they want to keep).
func (s *SiloClient) SetLockedFields(ctx context.Context, contentID string, fields []int) error {
	if fields == nil {
		fields = []int{}
	}
	return s.doJSON(ctx, http.MethodPatch, "/admin/items/"+url.PathEscape(contentID)+"/metadata",
		map[string]any{"locked_fields": fields}, nil)
}

// RefreshItem queues Silo's own manual metadata refresh for an item and
// returns the job id. A manual refresh replaces every unlocked field, so with
// the image lock removed Silo picks its default artwork again.
func (s *SiloClient) RefreshItem(ctx context.Context, contentID string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := s.doJSON(ctx, http.MethodPost, "/admin/items/"+url.PathEscape(contentID)+"/refresh-metadata",
		map[string]string{"mode": "quick"}, &out)
	return out.ID, err
}

// JobStatus returns an admin job's status: queued, running, completed,
// failed or cancelled.
func (s *SiloClient) JobStatus(ctx context.Context, jobID string) (string, string, error) {
	var out struct {
		Status       string `json:"status"`
		ErrorMessage string `json:"error_message"`
	}
	err := s.doJSON(ctx, http.MethodGet, "/admin/jobs/"+url.PathEscape(jobID), nil, &out)
	return out.Status, out.ErrorMessage, err
}

// Ping confirms the API key is accepted for admin routes.
func (s *SiloClient) Ping(ctx context.Context) error {
	return s.doJSON(ctx, http.MethodGet, "/admin/collections", nil, nil)
}

func sniffImageType(data []byte) string {
	switch ct := http.DetectContentType(data); ct {
	case "image/jpeg", "image/png", "image/webp":
		return ct
	default:
		return "image/jpeg"
	}
}
