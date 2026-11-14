package main

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/lichdotdev/lich/extension"
	"github.com/lichdotdev/lich/extension/i18n"
)

const (
	officialOwner = "lichdotdev"
	// An id starting with this is kept for officialOwner (docs/extensions.md,
	// Listing in the catalog).
	reservedPrefix = "lich"
	// GitHub's spdx_id for a license it could not identify.
	unknownLicense = "NOASSERTION"
)

var (
	repoPattern    = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	versionPattern = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)$`)
)

// report is what inspecting one repository found: the entry its latest
// release makes, and every problem that keeps it from being listed.
type report struct {
	repo     string
	entry    extension.CatalogEntry
	icon     []byte
	problems []string
	warnings []string
}

func (r *report) problem(format string, a ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, a...))
}

func (r *report) warn(format string, a ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, a...))
}

// inspect runs every check the catalog makes on one repository's latest
// release, short of what depends on the catalog's own records (settle). The
// error is GitHub's or the runner's, never the repository's.
func (b *builder) inspect(ctx context.Context, full string, categories map[string]bool) (*report, error) {
	r := &report{repo: full}
	if !repoPattern.MatchString(full) {
		r.problem("%q is not <owner>/<repo>", full)
		return r, nil
	}
	gh, err := b.repo(ctx, full)
	if repoProblem(err) {
		r.problem("repository: %v", err)
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	r.fromRepo(gh)
	rel, err := b.latestRelease(ctx, gh.FullName)
	if repoProblem(err) {
		r.problem("no published release: %v", err)
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	m := versionPattern.FindStringSubmatch(rel.Tag)
	if m == nil {
		r.problem("latest release %q is not tagged MAJOR.MINOR.PATCH", rel.Tag)
		return r, nil
	}
	r.entry.Tag, r.entry.Version, r.entry.Released = rel.Tag, m[1], rel.Published
	if r.entry.Commit, err = b.commit(ctx, gh.FullName, rel.Tag); err != nil {
		return nil, err
	}
	archives, err := b.archives(ctx, rel, r)
	if err != nil {
		return nil, err
	}
	r.fromArchives(archives)
	return r, b.listing(ctx, gh.FullName, r.entry.Commit, categories, r)
}

func (r *report) fromRepo(gh ghRepo) {
	if gh.Private {
		r.problem("the repository is private")
	}
	if gh.Fork {
		r.problem("the repository is a fork")
	}
	if gh.Archived {
		r.problem("the repository is archived")
	}
	r.entry.Publisher = gh.Owner.Login
	r.entry.Official = strings.EqualFold(gh.Owner.Login, officialOwner)
	r.entry.Stars = gh.Stars
	r.entry.Repo = "https://github.com/" + gh.FullName
	r.entry.RepoID = gh.ID
	if gh.License != nil && gh.License.SPDX != unknownLicense {
		r.entry.License = gh.License.SPDX
	}
}

// fromArchives fills the entry from the manifests, which must agree with
// their archive's name and the release's tag. The first archive, by
// platform, is the one read for everything else.
func (r *report) fromArchives(archives []archive) {
	r.entry.Builds = map[string]string{}
	for _, a := range archives {
		id := archivePattern.FindStringSubmatch(a.name)[1]
		if a.manifest.ID != id {
			r.problem("%s: the manifest's id is %q", a.name, a.manifest.ID)
		}
		if a.manifest.Version != r.entry.Version {
			r.problem("%s: the manifest says version %q, the tag %s", a.name, a.manifest.Version, r.entry.Tag)
		}
		r.entry.Builds[a.platform] = a.sha
	}
	if len(archives) == 0 {
		return
	}
	first := archives[0]
	m := first.manifest
	r.checkID(m.ID)
	if m.Description == "" {
		r.problem("the manifest sets no description")
	}
	if utf8.RuneCountInString(m.Description) > extension.MaxDescription {
		r.problem("the description is over %d characters", extension.MaxDescription)
	}
	r.entry.ID, r.entry.Name, r.entry.Description = m.ID, m.Name, m.Description
	r.entry.MinLichVersion, r.entry.Permissions = m.MinLichVersion, m.Permissions
	r.translations(first)
	if len(m.Surfaces) > 0 {
		s := m.Surfaces[0]
		r.entry.Glyph = s.Icon
		if f, ok := first.files[path.Clean(s.IconFile)]; ok && s.IconFile != "" {
			r.icon = f.Data
		}
	}
}

func (r *report) checkID(id string) {
	if strings.HasPrefix(id, reservedPrefix) && !r.entry.Official {
		r.problem("the id %q starts with %q, which is kept for %s", id, reservedPrefix, officialOwner)
	}
}

// translations reads manifest.name and manifest.description from every
// language's own catalog, never English's fallback.
func (r *report) translations(a archive) {
	bundle, err := i18n.Open(a.files)
	if err != nil {
		r.problem("%s: %v", a.name, err)
		return
	}
	texts := map[string]extension.CatalogText{}
	for _, tag := range bundle.Tags() {
		var t extension.CatalogText
		if m, found, ok := bundle.Lookup(tag, i18n.ManifestNameKey); ok && found == tag {
			t.Name = m.Text
		}
		if m, found, ok := bundle.Lookup(tag, i18n.ManifestDescriptionKey); ok && found == tag {
			t.Description = m.Text
		}
		if utf8.RuneCountInString(t.Description) > extension.MaxDescription {
			r.problem("%s: the %s description is over %d characters", a.name, tag, extension.MaxDescription)
		}
		if t != (extension.CatalogText{}) {
			texts[strings.ToLower(tag)] = t
		}
	}
	if len(texts) > 0 {
		r.entry.Translations = texts
	}
}
