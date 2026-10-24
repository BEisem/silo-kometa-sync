package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decode PNG posters for re-encoding
	"net/http"
	"sync"
	"time"
)

// Silo's "apply image" endpoint downloads from a URL and records that URL as
// the image source. Handing it a Plex URL would store the Plex token in
// Silo's database, so posters are relayed through this plugin's own
// /img/<nonce> route instead. Nonces are random, short-lived, and dropped once
// the apply call returns.
//
// That route's response travels over the plugin's gRPC connection, which
// carries at most 4 MiB, so larger images are re-encoded as JPEG first.
const (
	imageTTL       = 10 * time.Minute
	maxRelayBytes  = 3_800_000
	jpegReencodeQ  = 92
	nonceByteCount = 24
)

type imageEntry struct {
	data        []byte
	contentType string
	expires     time.Time
}

type imageRelay struct {
	mu      sync.Mutex
	entries map[string]imageEntry
}

func newImageRelay() *imageRelay {
	return &imageRelay{entries: map[string]imageEntry{}}
}

// put stores data and returns its nonce.
func (r *imageRelay) put(data []byte) (string, error) {
	data, err := fitForRelay(data)
	if err != nil {
		return "", err
	}
	buf := make([]byte, nonceByteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(buf)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	r.entries[nonce] = imageEntry{data: data, contentType: sniffImageType(data), expires: time.Now().Add(imageTTL)}
	return nonce, nil
}

func (r *imageRelay) get(nonce string) (imageEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[nonce]
	if !ok || time.Now().After(e.expires) {
		delete(r.entries, nonce)
		return imageEntry{}, false
	}
	return e, true
}

func (r *imageRelay) drop(nonce string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, nonce)
}

func (r *imageRelay) sweepLocked() {
	now := time.Now()
	for k, e := range r.entries {
		if now.After(e.expires) {
			delete(r.entries, k)
		}
	}
}

// fitForRelay re-encodes images too large for the relay as JPEG.
func fitForRelay(data []byte) ([]byte, error) {
	if len(data) <= maxRelayBytes {
		return data, nil
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png":
	default:
		return nil, fmt.Errorf("image is %d bytes and not JPEG/PNG, cannot shrink it below the relay limit", len(data))
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode oversized image: %w", err)
	}
	for _, q := range []int{jpegReencodeQ, 85, 75} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, fmt.Errorf("re-encode image: %w", err)
		}
		if buf.Len() <= maxRelayBytes {
			return buf.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("image still over %d bytes after re-encoding", maxRelayBytes)
}
