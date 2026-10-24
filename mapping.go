package main

import (
	"regexp"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"strconv"
	"strings"
)

// Collections from each Plex library go into one Silo library. Silo lets a
// collection hold only items from its own libraries, and the catalog lookup
// reports just one library per title, so without this a "TV Shows 4k"
// collection could land in the regular TV library.

// siloLibrary is a Silo library as the plugin host reports it.
type siloLibrary struct {
	ID   int
	Name string
	Type string // "movie", "tv" or "mixed"
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func normName(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

func compatible(sec plexSection, lib siloLibrary) bool {
	switch lib.Type {
	case "mixed", "":
		return true
	case "movie":
		return sec.Type == "movie"
	case "tv", "series", "show":
		return sec.Type == "show"
	}
	return false
}

// compatibleLibraries lists the Silo libraries a Plex library's collections
// may go into.
func compatibleLibraries(sec plexSection, libs []siloLibrary) []siloLibrary {
	var out []siloLibrary
	for _, l := range libs {
		if compatible(sec, l) {
			out = append(out, l)
		}
	}
	return out
}

// autoMatch picks a Silo library for a Plex library: the same name (ignoring
// case, spaces and punctuation), else the only compatible library that is
// also 4K or also not 4K ("TV Shows" -> "TV", "TV Shows 4k" -> "TV 4K").
// It returns 0 when nothing matches unambiguously.
func autoMatch(sec plexSection, libs []siloLibrary) int {
	cands := compatibleLibraries(sec, libs)
	want := normName(sec.Title)
	for _, l := range cands {
		if normName(l.Name) == want {
			return l.ID
		}
	}
	match := 0
	for _, l := range cands {
		if (libraryNameRank(l.Name) > 0) == (libraryNameRank(sec.Title) > 0) {
			if match != 0 {
				return 0
			}
			match = l.ID
		}
	}
	return match
}

// targetLibrary resolves where a Plex library's collections go: an explicit
// choice if it still exists and fits, else the automatic match.
func targetLibrary(sec plexSection, libs []siloLibrary, explicit map[string]int) int {
	if id, ok := explicit[sectionID(sec)]; ok && id > 0 {
		for _, l := range libs {
			if l.ID == id && compatible(sec, l) {
				return id
			}
		}
	}
	return autoMatch(sec, libs)
}

func libraryName(libs []siloLibrary, id int) string {
	for _, l := range libs {
		if l.ID == id {
			return l.Name
		}
	}
	return "library " + strconv.Itoa(id)
}

// hostLibraries converts the plugin host's library list.
func hostLibraries(libs []*pluginv1.Library) []siloLibrary {
	out := make([]siloLibrary, 0, len(libs))
	for _, l := range libs {
		id, err := strconv.Atoi(l.GetId())
		if err != nil {
			continue
		}
		out = append(out, siloLibrary{ID: id, Name: l.GetName(), Type: l.GetMediaType()})
	}
	return out
}
