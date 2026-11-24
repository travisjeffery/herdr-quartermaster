package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var linearToken = regexp.MustCompile(`[A-Z][A-Z0-9]*-\d+`)

var notLinear = []string{"SHA", "UTF", "ISO", "RFC", "HTTP", "TLS"}

// linearKey is the Linear issue a bead names: the first key in its title, else
// the first in its notes. With prefixes set only those teams count; without,
// a denylist keeps the commonest look-alikes out.
func linearKey(b Bead, prefixes []string) string {
	// Notes mention other issues in passing ("blocked by ENG-900"); without a
	// prefix filter, only a note that names the bead's issue counts.
	notes := b.Notes
	if len(prefixes) == 0 {
		notes = linearNoteLines(b.Notes)
	}
	for _, text := range []string{b.Title, notes} {
		for _, m := range linearToken.FindAllStringIndex(text, -1) {
			key := text[m[0]:m[1]]
			if !standalone(text, m[0], m[1]) {
				continue
			}
			team := key[:strings.Index(key, "-")]
			ok := !slices.Contains(notLinear, team)
			if len(prefixes) > 0 {
				ok = slices.Contains(prefixes, team)
			}
			if ok {
				return key
			}
		}
	}
	return ""
}

// standalone rejects tokens that are part of something longer: US-EAST-2,
// RSA-OAEP-256, a-zA-Z0-9, GLM-5.3.
func standalone(s string, start, end int) bool {
	if start > 0 && (isAlnum(s[start-1]) || s[start-1] == '-') {
		return false
	}
	if end < len(s) && (isAlnum(s[end]) || s[end] == '-') {
		return false
	}
	return !(end+1 < len(s) && s[end] == '.' && s[end+1] >= '0' && s[end+1] <= '9')
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func titledWith(title, key string) bool {
	rest, ok := strings.CutPrefix(title, key)
	return ok && (rest == "" || rest[0] < '0' || rest[0] > '9')
}

// linearMoves is where each thread's Linear issue should be, going by its PR.
func linearMoves(threads []Thread) []string {
	var out []string
	for _, t := range threads {
		if t.Linear == "" || t.PR == nil {
			continue
		}
		switch {
		case t.PR.State == "MERGED" && t.OpenChildren == 0:
			out = append(out, fmt.Sprintf("%s → Done (PR #%d merged)", t.Linear, t.PR.Number))
		case t.PR.State == "OPEN" && !t.PR.IsDraft:
			out = append(out, fmt.Sprintf("%s → In Review (PR #%d open)", t.Linear, t.PR.Number))
		}
		// A PR cached before titles were fetched has none yet; don't flag it.
		if t.PR.State == "OPEN" && t.PR.Title != "" && !titledWith(t.PR.Title, t.Linear) {
			out = append(out, fmt.Sprintf("PR #%d title lacks %s", t.PR.Number, t.Linear))
		}
	}
	return out
}

func linearNoteLines(notes string) string {
	var out []string
	for _, l := range strings.Split(notes, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "linear:") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
