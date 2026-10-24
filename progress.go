package main

import (
	"sync"
	"time"
)

// syncProgress is what the status page polls while a sync or revert runs.
// All methods are safe on a nil receiver, so tests can leave it unset.
type syncProgress struct {
	mu      sync.Mutex
	phase   string
	detail  string
	done    int
	total   int
	started time.Time
}

type progressView struct {
	Running bool   `json:"running"`
	Mode    string `json:"mode,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Started string `json:"started,omitempty"`
}

func (p *syncProgress) start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase, p.detail, p.done, p.total, p.started = "Starting", "", 0, 0, time.Now()
}

// set begins a new step with a known amount of work.
func (p *syncProgress) set(phase string, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase, p.detail, p.done, p.total = phase, "", 0, total
}

func (p *syncProgress) detailf(d string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.detail = d
}

func (p *syncProgress) add(n int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
}

func (p *syncProgress) snapshot() progressView {
	if p == nil {
		return progressView{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v := progressView{Phase: p.phase, Detail: p.detail, Done: p.done, Total: p.total}
	if !p.started.IsZero() {
		v.Started = p.started.UTC().Format(time.RFC3339)
	}
	return v
}
