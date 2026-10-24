package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"crypto/subtle"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// Library selection lives on the status page as a checklist, because Silo's
// settings form can only fill option lists for request-router plugins. It is
// saved in the plugin's own instance state, keyed by Plex's library UUID so
// renaming a library in Plex doesn't drop it.
//
// Until the checklist is first saved, the older comma-separated "Plex
// libraries" setting still applies (blank there means every library). Once
// saved, the checklist is the only source, and an empty checklist syncs
// nothing.

const selectionKey = "libraries"

type librarySelection struct {
	IDs      []string       `json:"ids"`
	Winner   string         `json:"winner,omitempty"`          // see winner* constants
	Priority map[string]int `json:"priority,omitempty"`        // library UUID -> 1 = first
	Mapping  map[string]int `json:"mapping,omitempty"`         // library UUID -> Silo library for collections (0/absent = automatic)
	CollMode string         `json:"collection_mode,omitempty"` // "separate" (default) or "combined"
	SavedAt  string         `json:"saved_at"`
}

// How to choose when several libraries hold the same Silo item.
const (
	winnerHighestRes = "highest"
	winnerLowestRes  = "lowest"
	winnerPriority   = "priority"
)

func normalizeWinner(w string) string {
	switch w {
	case winnerLowestRes, winnerPriority:
		return w
	}
	return winnerHighestRes
}

// loadPolicy returns the saved duplicate policy (highest resolution when
// never saved).
func loadPolicy(ctx context.Context, io stateIO) (librarySelection, error) {
	var s librarySelection
	data, found, err := io.ReadInstanceState(ctx, selectionKey)
	if err != nil || !found || len(data) == 0 {
		s.Winner = winnerHighestRes
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	s.Winner = normalizeWinner(s.Winner)
	return s, nil
}

// rankFunc scores a Plex copy; the highest score wins a shared Silo item.
func (s librarySelection) rankFunc() func(plexSection, plexItem) int {
	switch s.Winner {
	case winnerLowestRes:
		return func(sec plexSection, it plexItem) int { return -itemRank(sec, it) }
	case winnerPriority:
		return func(sec plexSection, _ plexItem) int {
			if p, ok := s.Priority[sectionID(sec)]; ok && p > 0 {
				return -p
			}
			return -1000 // unranked libraries lose to ranked ones
		}
	}
	return itemRank
}

// signature identifies the policy, so a change re-pushes shared items once.
func (s librarySelection) signature() string {
	if s.Winner != winnerPriority {
		return s.Winner
	}
	ids := make([]string, 0, len(s.Priority))
	for id := range s.Priority {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString(winnerPriority)
	for _, id := range ids {
		b.WriteString("|" + id + "=" + strconv.Itoa(s.Priority[id]))
	}
	return b.String()
}

// loadSelection returns the saved checklist; explicit is false when it has
// never been saved.
func loadSelection(ctx context.Context, io stateIO) (sel map[string]bool, explicit bool, err error) {
	data, found, err := io.ReadInstanceState(ctx, selectionKey)
	if err != nil || !found || len(data) == 0 {
		return nil, false, err
	}
	var s librarySelection
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, false, err
	}
	sel = map[string]bool{}
	for _, id := range s.IDs {
		sel[id] = true
	}
	return sel, true, nil
}

func saveSelection(ctx context.Context, io stateIO, ids []string) error {
	return savePolicy(ctx, io, librarySelection{IDs: ids, Winner: winnerHighestRes})
}

func savePolicy(ctx context.Context, io stateIO, sel librarySelection) error {
	sort.Strings(sel.IDs)
	sel.Winner = normalizeWinner(sel.Winner)
	sel.SavedAt = nowRFC3339()
	data, err := json.Marshal(sel)
	if err != nil {
		return err
	}
	return io.WriteInstanceState(ctx, selectionKey, data)
}

// selector decides which Plex libraries a run includes.
func selector(cfg Config, sel map[string]bool, explicit bool) func(plexSection) bool {
	if explicit {
		return func(sec plexSection) bool { return sel[sectionID(sec)] }
	}
	return func(sec plexSection) bool { return cfg.wantsLibrary(sec.Title) }
}

// currentSelector loads the saved selection for this run.
func currentSelector(ctx context.Context, cfg Config) (func(plexSection) bool, error) {
	host := sdkruntime.Host()
	if host == nil {
		return selector(cfg, nil, false), nil
	}
	sel, explicit, err := loadSelection(ctx, host)
	if err != nil {
		return nil, err
	}
	return selector(cfg, sel, explicit), nil
}

type libraryOption struct {
	ID       string
	Title    string
	Kind     string // "Movies" or "TV"
	Selected bool
	Priority int // 1-based position for the "library priority" choice

	// Where this library's collections go in Silo.
	MapChoices []siloLibrary // compatible Silo libraries
	Mapped     int           // explicit choice; 0 = automatic
	AutoName   string        // what "Automatic" resolves to; empty when nothing matches
}

// libraryView is what the Libraries card shows.
type libraryView struct {
	Options   []libraryOption
	Winner    string
	Positions []int
	MapError  string // Silo's library list couldn't be read
	CollMode  string
}

// libraryOptions lists Plex's movie and TV libraries with their current
// selection and duplicate policy, for the status page.
func libraryOptions(ctx context.Context, cfg Config) (*libraryView, string) {
	if cfg.PlexURL == "" || cfg.PlexToken == "" {
		return nil, "Enter the Plex URL and token in the plugin settings to list libraries."
	}
	sections, err := newPlexClient(cfg.PlexURL, cfg.PlexToken).Sections(ctx)
	if err != nil {
		return nil, "Couldn't list Plex libraries: " + err.Error()
	}
	pick, err := currentSelector(ctx, cfg)
	if err != nil {
		return nil, "Couldn't read the saved selection: " + err.Error()
	}
	policy := librarySelection{Winner: winnerHighestRes}
	if host := sdkruntime.Host(); host != nil {
		if p, err := loadPolicy(ctx, host); err == nil {
			policy = p
		}
	}
	view := &libraryView{Winner: policy.Winner, CollMode: normalizeCollectionMode(policy.CollMode)}
	var siloLibs []siloLibrary
	if host := sdkruntime.Host(); host != nil {
		if libs, err := host.ListLibraries(ctx, ""); err == nil {
			siloLibs = hostLibraries(libs)
		} else {
			view.MapError = "Couldn't read Silo's libraries: " + err.Error()
		}
	}
	for _, sec := range sections {
		kind := "Movies"
		if sec.Type == "show" {
			kind = "TV"
		}
		opt := libraryOption{ID: sectionID(sec), Title: sec.Title, Kind: kind,
			Selected: pick(sec), Priority: policy.Priority[sectionID(sec)],
			MapChoices: compatibleLibraries(sec, siloLibs), Mapped: policy.Mapping[sectionID(sec)]}
		if auto := autoMatch(sec, siloLibs); auto > 0 {
			opt.AutoName = libraryName(siloLibs, auto)
		}
		view.Options = append(view.Options, opt)
	}
	// Default priority: 4K-named libraries first, then Plex order.
	sort.SliceStable(view.Options, func(i, j int) bool {
		pi, pj := view.Options[i].Priority, view.Options[j].Priority
		if pi > 0 && pj > 0 {
			return pi < pj
		}
		if (pi > 0) != (pj > 0) {
			return pi > 0
		}
		return libraryNameRank(view.Options[i].Title) > libraryNameRank(view.Options[j].Title)
	})
	for i := range view.Options {
		view.Options[i].Priority = i + 1
		view.Positions = append(view.Positions, i+1)
	}
	return view, ""
}

// saveLibraries handles the Libraries card's Save button.
func (r *routesServer) saveLibraries(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	p := r.p
	form, err := url.ParseQuery(string(req.GetBody()))
	if err != nil {
		return htmlMessage(http.StatusBadRequest, "Could not read the form.")
	}
	if subtle.ConstantTimeCompare([]byte(form.Get("token")), []byte(p.token())) != 1 {
		return htmlMessage(http.StatusForbidden, "This form has expired. Reload the status page and try again.")
	}
	host := sdkruntime.Host()
	if host == nil {
		return htmlMessage(http.StatusServiceUnavailable, "Silo host connection is not ready yet. Try again in a moment.")
	}
	ids := []string{}
	for _, id := range form["library"] {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	sel := librarySelection{IDs: ids, Winner: normalizeWinner(form.Get("winner")), Priority: map[string]int{}, Mapping: map[string]int{},
		CollMode: normalizeCollectionMode(form.Get("collection_mode"))}
	for key, vals := range form {
		if len(vals) == 0 {
			continue
		}
		n, err := strconv.Atoi(vals[0])
		if err != nil || n <= 0 {
			continue
		}
		if id, ok := strings.CutPrefix(key, "priority_"); ok {
			sel.Priority[id] = n
		}
		if id, ok := strings.CutPrefix(key, "map_"); ok {
			sel.Mapping[id] = n
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := savePolicy(ctx, host, sel); err != nil {
		return htmlMessage(http.StatusInternalServerError, "Couldn't save: "+err.Error())
	}
	if len(ids) == 0 {
		return htmlMessage(http.StatusOK, "Saved. No libraries are selected, so syncs won't change anything.")
	}
	return htmlMessage(http.StatusOK, "Saved. The next sync uses these settings.")
}
