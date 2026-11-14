package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/lichdotdev/lich/extension"
)

// The files of this repository. A maintainer edits the first three and
// identities.json; the build writes the rest.
const (
	extensionsFile = "extensions.json"
	revokedFile    = "revoked.json"
	categoriesFile = "categories.json"
	identitiesFile = "identities.json"
	lockFile       = "releases.lock.json"
	catalogFile    = "catalog.json"
	iconsDir       = "icons"
	// Where catalog.json and icons/ are read from (docs/extensions.md,
	// Listing in the catalog).
	publishedBase = "https://raw.githubusercontent.com/lichdotdev/extensions/main/"
)

// lockEntry is a version as the catalog first saw it. It never changes.
type lockEntry struct {
	Commit string            `json:"commit"`
	Builds map[string]string `json:"builds"`
}

// identity is the repository an id was first listed from, by GitHub's
// numeric id, which a rename keeps and a recreation does not.
type identity struct {
	Repo   string `json:"repo"`
	RepoID int64  `json:"repoId"`
}

type state struct {
	dir        string
	repos      []string
	revoked    []extension.Revocation
	categories []extension.CatalogCategory
	previous   extension.Catalog
	lock       map[string]lockEntry
	identities map[string]identity
}

func loadState(dir string) (*state, error) {
	s := &state{dir: dir, lock: map[string]lockEntry{}, identities: map[string]identity{}}
	files := []struct {
		name     string
		v        any
		optional bool
	}{
		{extensionsFile, &s.repos, false},
		{revokedFile, &s.revoked, false},
		{categoriesFile, &s.categories, false},
		{identitiesFile, &s.identities, true},
		{lockFile, &s.lock, true},
		{catalogFile, &s.previous, true},
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if f.optional && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, f.v); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return s, nil
}

func (s *state) categorySet() map[string]bool {
	set := map[string]bool{}
	for _, c := range s.categories {
		set[c.ID] = true
	}
	return set
}

func (s *state) revokedFor(repo string, repoID int64) *extension.Revocation {
	for i, rv := range s.revoked {
		if strings.EqualFold(rv.Repo, repo) || (repoID != 0 && rv.RepoID == repoID) {
			return &s.revoked[i]
		}
	}
	return nil
}

func (s *state) previousOf(repo string, repoID int64) *extension.CatalogEntry {
	for i, e := range s.previous.Extensions {
		if strings.EqualFold(e.Repo, repo) || (repoID != 0 && e.RepoID == repoID) {
			return &s.previous.Extensions[i]
		}
	}
	return nil
}

func lockKey(e extension.CatalogEntry) string { return e.ID + "@" + e.Version }

// settle adds the problems that depend on the catalog's own records: who owns
// the id, and what the version's archives were when first seen. taken is
// every id listed so far, by the repository it is listed from.
func (s *state) settle(r *report, taken map[string]string) {
	e := r.entry
	if e.ID == "" {
		return
	}
	if id, ok := s.identities[e.ID]; ok && id.RepoID != e.RepoID {
		r.problem("the id %q was listed from %s (GitHub id %d), and this repository's GitHub id is %d: "+
			"a maintainer confirms the change in %s", e.ID, id.Repo, id.RepoID, e.RepoID, identitiesFile)
	} else if owner, ok := taken[e.ID]; ok && !strings.EqualFold(owner, e.Repo) {
		r.problem("the id %q is already listed from %s", e.ID, owner)
	}
	// A partial release would differ from its lock for a reason already named.
	if len(r.problems) > 0 {
		return
	}
	if l, ok := s.lock[lockKey(e)]; ok && (l.Commit != e.Commit || !maps.Equal(l.Builds, e.Builds)) {
		r.problem("%s is not what the catalog recorded the first time it saw it "+
			"(commit %s and its archives' sha256 in %s): a release is never replaced, publish a new version",
			lockKey(e), l.Commit, lockFile)
	}
}

// accept is the entry r lists: its own when nothing holds it, recorded in the
// lock and identities, or else the last good one, held with the reasons.
func (s *state) accept(r *report, listed string) (extension.CatalogEntry, bool) {
	if len(r.problems) == 0 {
		e := r.entry
		if _, ok := s.lock[lockKey(e)]; !ok {
			s.lock[lockKey(e)] = lockEntry{Commit: e.Commit, Builds: e.Builds}
		}
		s.identities[e.ID] = identity{Repo: e.Repo, RepoID: e.RepoID}
		return e, true
	}
	prev := s.previousOf(listed, r.entry.RepoID)
	if prev == nil {
		return extension.CatalogEntry{}, false
	}
	held := *prev
	held.Held = strings.Join(r.problems, "; ")
	return held, true
}

// save writes catalog.json, the lock, the identities and icons/: an icon
// for each fresh entry, the one already there for a held one.
func (s *state) save(cat extension.Catalog, icons map[string][]byte) error {
	for name, v := range map[string]any{catalogFile: cat, lockFile: s.lock, identitiesFile: s.identities} {
		if err := writeJSON(filepath.Join(s.dir, name), v); err != nil {
			return err
		}
	}
	dir := filepath.Join(s.dir, iconsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for id, png := range icons {
		if err := os.WriteFile(filepath.Join(dir, id+".png"), png, 0o644); err != nil {
			return err
		}
	}
	listed := map[string]bool{}
	for _, e := range cat.Extensions {
		listed[e.ID+".png"] = listed[e.ID+".png"] || e.Icon != ""
	}
	stale, err := filepath.Glob(filepath.Join(dir, "*.png"))
	if err != nil {
		return err
	}
	for _, f := range stale {
		if !listed[filepath.Base(f)] {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeJSON(file string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(file, buf.Bytes(), 0o644)
}

// sameListing is true when a and b differ in nothing but when they were
// generated, so an hour with no change commits nothing.
func sameListing(a, b extension.Catalog) bool {
	a.Generated, b.Generated = "", ""
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}
