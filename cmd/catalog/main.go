// Command catalog builds catalog.json from the repositories extensions.json
// lists, and checks one repository the way the build would.
//
//	catalog build
//	catalog check <owner>/<repo>
//
// GITHUB_TOKEN, when set, authenticates the GitHub API calls; LICH_BIN names
// the lich that runs `lich extension check` (default lich, from PATH).
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lichdotdev/lich/extension"
)

const usage = "usage: catalog build | catalog check <owner>/<repo>"

type builder struct {
	api, raw string
	token    string
	lich     string
	http     *http.Client
}

func main() {
	lich := os.Getenv("LICH_BIN")
	if lich == "" {
		lich = "lich"
	}
	b := &builder{
		api: "https://api.github.com", raw: "https://raw.githubusercontent.com",
		token: os.Getenv("GITHUB_TOKEN"), lich: lich,
		// Long enough for the largest archive lich accepts.
		http: &http.Client{Timeout: 10 * time.Minute},
	}
	os.Exit(run(context.Background(), b, ".", os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, b *builder, dir string, args []string, stdout, stderr io.Writer) int {
	var err error
	ok := true
	switch {
	case len(args) == 1 && args[0] == "build":
		err = b.build(ctx, dir, stdout)
	case len(args) == 2 && args[0] == "check":
		ok, err = b.check(ctx, dir, args[1], stdout)
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

func (b *builder) build(ctx context.Context, dir string, log io.Writer) error {
	s, err := loadState(dir)
	if err != nil {
		return err
	}
	categories := s.categorySet()
	taken := map[string]string{}
	entries := []extension.CatalogEntry{}
	icons := map[string][]byte{}
	for _, full := range s.repos {
		r, err := b.inspect(ctx, full, categories)
		if err != nil {
			return fmt.Errorf("%s: %w", full, err)
		}
		listed := "https://github.com/" + full
		if rv := s.revokedFor(listed, r.entry.RepoID); rv != nil {
			fmt.Fprintf(log, "%s: revoked: %s\n", full, rv.Reason)
			continue
		}
		s.settle(r, taken)
		printReport(log, r)
		e, ok := s.accept(r, listed)
		if !ok {
			continue
		}
		if e.Held == "" && r.icon != nil {
			e.Icon = publishedBase + iconsDir + "/" + e.ID + ".png"
			icons[e.ID] = r.icon
		}
		taken[e.ID] = e.Repo
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	cat := extension.Catalog{
		FormatVersion: extension.CatalogFormat,
		Generated:     time.Now().UTC().Format(time.RFC3339),
		Categories:    s.categories,
		Extensions:    entries,
		Revoked:       s.revoked,
	}
	if sameListing(cat, s.previous) {
		cat.Generated = s.previous.Generated
	}
	return s.save(cat, icons)
}

// check runs the build's checks on one repository against the catalog as it
// stands, and prints every problem. It is false when any holds the listing.
func (b *builder) check(ctx context.Context, dir, full string, out io.Writer) (bool, error) {
	s, err := loadState(dir)
	if err != nil {
		return false, err
	}
	r, err := b.inspect(ctx, full, s.categorySet())
	if err != nil {
		return false, err
	}
	if rv := s.revokedFor("https://github.com/"+full, r.entry.RepoID); rv != nil {
		r.problem("revoked: %s", rv.Reason)
	}
	taken := map[string]string{}
	for _, e := range s.previous.Extensions {
		taken[e.ID] = e.Repo
	}
	s.settle(r, taken)
	printReport(out, r)
	return len(r.problems) == 0, nil
}

func printReport(w io.Writer, r *report) {
	for _, p := range r.problems {
		fmt.Fprintf(w, "%s: problem: %s\n", r.repo, p)
	}
	for _, m := range r.warnings {
		fmt.Fprintf(w, "%s: warning: %s\n", r.repo, m)
	}
	if len(r.problems) == 0 {
		fmt.Fprintf(w, "%s: ok: %s %s, %s\n", r.repo, r.entry.ID, r.entry.Version, strings.Join(platforms(r.entry), ", "))
	}
}

func platforms(e extension.CatalogEntry) []string {
	var out []string
	for p := range e.Builds {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
