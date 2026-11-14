package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"path"
	"unicode/utf8"

	"github.com/lichdotdev/lich/extension"
)

const (
	listingDir = "listing"
	// A repository without listing/listing.json is listed here
	// (docs/extensions.md, What the catalog shows).
	defaultCategory = "tools"
)

// frame is what a picture of the listing is held to (docs/extensions.md, What
// the catalog shows). A zero maximum is no maximum.
type frame struct {
	what                   string
	minW, minH, maxW, maxH int
	maxBytes               int64
}

var (
	coverFrame      = frame{"cover", 1600, 1000, 3200, 2000, extension.MaxCoverBytes}
	screenshotFrame = frame{"screenshot", 1280, 800, 0, 0, extension.MaxScreenshotBytes}
)

// listing reads listing/listing.json at the release's commit and fills the
// entry's categories, cover and screenshots.
func (b *builder) listing(ctx context.Context, full, commit string, categories map[string]bool, r *report) error {
	body, err := b.fetch(ctx, b.rawURL(full, commit, extension.ListingFile), false, maxJSONBytes)
	if errors.Is(err, errNotFound) {
		r.warn("no %s: listed under %s, with no cover or screenshots", extension.ListingFile, defaultCategory)
		r.entry.Categories = []string{defaultCategory}
		return nil
	}
	if errors.Is(err, errTooLarge) {
		r.problem("%v", err)
		return nil
	}
	if err != nil {
		return err
	}
	var l extension.Listing
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		r.problem("%s: %v", extension.ListingFile, err)
		return nil
	}
	checkCategories(l.Categories, categories, r)
	r.entry.Categories = l.Categories
	if l.Cover != "" {
		u, err := b.picture(ctx, full, commit, l.Cover, coverFrame, r)
		if err != nil {
			return err
		}
		r.entry.Cover = u
	}
	return b.screenshots(ctx, full, commit, l.Screenshots, r)
}

func checkCategories(names []string, known map[string]bool, r *report) {
	if len(names) == 0 || len(names) > extension.MaxCategories {
		r.problem("%s: name one or two categories, not %d", extension.ListingFile, len(names))
	}
	seen := map[string]bool{}
	for _, c := range names {
		if !known[c] {
			r.problem("%s: %q is not a category of the catalog", extension.ListingFile, c)
		}
		if seen[c] {
			r.problem("%s: %q is named twice", extension.ListingFile, c)
		}
		seen[c] = true
	}
}

func (b *builder) screenshots(ctx context.Context, full, commit string, shots []extension.ListingImage, r *report) error {
	if len(shots) > extension.MaxScreenshots {
		r.problem("%s: %d screenshots, at most %d", extension.ListingFile, len(shots), extension.MaxScreenshots)
	}
	for _, s := range shots {
		if s.Alt == "" || utf8.RuneCountInString(s.Alt) > extension.MaxAlt {
			r.problem("%s: %s: alt is required, at most %d characters", extension.ListingFile, s.File, extension.MaxAlt)
		}
		u, err := b.picture(ctx, full, commit, s.File, screenshotFrame, r)
		if err != nil {
			return err
		}
		if u != "" {
			r.entry.Screenshots = append(r.entry.Screenshots, extension.CatalogImage{URL: u, Alt: s.Alt})
		}
	}
	return nil
}

// picture checks one image of listing/ and returns its URL pinned to commit,
// or "" with a problem on r.
func (b *builder) picture(ctx context.Context, full, commit, file string, f frame, r *report) (string, error) {
	name := path.Join(listingDir, file)
	if file == "" || file == "." || !fs.ValidPath(file) {
		r.problem("%s: %s %q must be a path inside %s/", extension.ListingFile, f.what, file, listingDir)
		return "", nil
	}
	u := b.rawURL(full, commit, name)
	body, err := b.fetch(ctx, u, false, f.maxBytes)
	if repoProblem(err) {
		r.problem("%s: %v", name, err)
		return "", nil
	}
	if err != nil {
		return "", err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || (format != "png" && format != "jpeg") {
		r.problem("%s: a %s is PNG or JPEG", name, f.what)
		return "", nil
	}
	if !f.fits(cfg.Width, cfg.Height) {
		r.problem("%s: %d × %d does not fit a %s: %s", name, cfg.Width, cfg.Height, f.what, f.rule())
		return "", nil
	}
	return u, nil
}

func (f frame) fits(w, h int) bool {
	if w*10 != h*16 || w < f.minW || h < f.minH {
		return false
	}
	return f.maxW == 0 || (w <= f.maxW && h <= f.maxH)
}

func (f frame) rule() string {
	if f.maxW == 0 {
		return fmt.Sprintf("16:10, at least %d × %d", f.minW, f.minH)
	}
	return fmt.Sprintf("16:10, from %d × %d to %d × %d", f.minW, f.minH, f.maxW, f.maxH)
}
