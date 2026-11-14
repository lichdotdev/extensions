package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lichdotdev/lich/extension"
)

const (
	notesRepo   = "lichdotdev/lich-notes"
	notesRepoID = 1001
	notesCommit = "0123456789abcdef0123456789abcdef01234567"
	// fakeLichEnv makes the test binary act as lich; fakeLichFail names the
	// archive it fails.
	fakeLichEnv  = "CATALOG_FAKE_LICH"
	fakeLichFail = "CATALOG_FAKE_LICH_FAIL"
)

// TestMain is also the fake lich: `extension check <archive>` exits 1 for the
// archive fakeLichFail names and 0 for any other.
func TestMain(m *testing.M) {
	if os.Getenv(fakeLichEnv) != "" {
		if len(os.Args) != 4 || os.Args[1] != "extension" || os.Args[2] != "check" {
			fmt.Println("unexpected arguments", os.Args[1:])
			os.Exit(2)
		}
		if fail := os.Getenv(fakeLichFail); fail != "" && filepath.Base(os.Args[3]) == fail {
			fmt.Println("manifest.json: executable is missing")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeGitHub serves the API, release downloads and raw files from paths.
type fakeGitHub struct {
	mu    sync.Mutex
	paths map[string][]byte
	srv   *httptest.Server
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body, ok := f.paths[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(body)
}

func (f *fakeGitHub) set(path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths[path] = body
}

func (f *fakeGitHub) setJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	f.set(path, data)
}

// fixture is a catalog checkout and the GitHub it reads.
type fixture struct {
	t   *testing.T
	gh  *fakeGitHub
	dir string
	b   *builder
}

func newFixture(t *testing.T, repos ...string) *fixture {
	t.Helper()
	gh := &fakeGitHub{paths: map[string][]byte{}}
	gh.srv = httptest.NewServer(gh)
	t.Cleanup(gh.srv.Close)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeLichEnv, "1")
	dir := t.TempDir()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	categories, err := os.ReadFile(filepath.Join(root, "..", "..", categoriesFile))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, gh: gh, dir: dir, b: &builder{
		api: gh.srv.URL + "/api", raw: gh.srv.URL + "/raw", lich: exe, http: gh.srv.Client(),
	}}
	f.write(categoriesFile, string(categories))
	f.write(revokedFile, "[]")
	repoList, _ := json.Marshal(repos)
	f.write(extensionsFile, string(repoList))
	return f
}

func (f *fixture) write(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// release is one published release of a fixture repository.
type release struct {
	repo     string
	repoID   int64
	tag      string
	manifest extension.Manifest
	// platforms default to linux_amd64 and darwin_arm64.
	platforms []string
	locales   map[string]string
	listing   string
	images    map[string][]byte
	// sums replaces checksums.txt's line for an archive.
	sums map[string]string
	// salt changes every archive's bytes, for a release replaced in place.
	salt string
}

func notes() release {
	return release{
		repo: notesRepo, repoID: notesRepoID, tag: "v0.2.0",
		manifest: extension.Manifest{
			FormatVersion: 1, ID: "lich-notes", Name: "LichNotes", Version: "0.2.0",
			Description: "Browse, read and edit the Markdown notes in a folder of yours.",
			Executable:  "lich-notes",
			Surfaces:    []extension.Surface{{ID: "main", Title: "Notes", Icon: "notebook-pen", IconFile: "icon.png"}},
		},
		locales: map[string]string{
			"pt-br": `{"manifest": {"name": "LichNotes", "description": "Navegue e edite as notas de uma pasta sua."}}`,
		},
		listing: `{"categories": ["notes"], "cover": "cover.png",
			"screenshots": [{"file": "screenshots/list.png", "alt": "The list of notes"}]}`,
		images: map[string][]byte{"cover.png": pngOf(1600, 1000), "screenshots/list.png": pngOf(1280, 800)},
	}
}

func pngOf(w, h int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func (f *fixture) publish(rel release) {
	t := f.t
	t.Helper()
	owner, _, _ := strings.Cut(rel.repo, "/")
	f.gh.setJSON(t, "/api/repos/"+rel.repo, map[string]any{
		"id": rel.repoID, "full_name": rel.repo, "owner": map[string]string{"login": owner},
		"stargazers_count": 42, "license": map[string]string{"spdx_id": "AGPL-3.0"},
	})
	f.gh.setJSON(t, "/api/repos/"+rel.repo+"/commits/"+rel.tag, map[string]string{"sha": notesCommit})
	platforms := rel.platforms
	if platforms == nil {
		platforms = []string{"linux_amd64", "darwin_arm64"}
	}
	var sums strings.Builder
	assets := []map[string]string{}
	for _, p := range platforms {
		name := rel.manifest.ID + "_" + p + ".tar.gz"
		data := archiveOf(t, rel)
		f.gh.set("/dl/"+name, data)
		sum := sha(data)
		if s, ok := rel.sums[name]; ok {
			sum = s
		}
		fmt.Fprintf(&sums, "%s  %s\n", sum, name)
		assets = append(assets, map[string]string{"name": name, "browser_download_url": f.gh.srv.URL + "/dl/" + name})
	}
	f.gh.set("/dl/checksums.txt", []byte(sums.String()))
	assets = append(assets, map[string]string{"name": checksumsAsset, "browser_download_url": f.gh.srv.URL + "/dl/checksums.txt"})
	f.gh.setJSON(t, "/api/repos/"+rel.repo+"/releases/latest", map[string]any{
		"tag_name": rel.tag, "published_at": "2026-11-13T10:00:00Z", "assets": assets,
	})
	raw := "/raw/" + rel.repo + "/" + notesCommit + "/listing/"
	if rel.listing != "" {
		f.gh.set(raw+"listing.json", []byte(rel.listing))
	}
	for name, data := range rel.images {
		f.gh.set(raw+name, data)
	}
}

func archiveOf(t *testing.T, rel release) []byte {
	t.Helper()
	manifest, err := json.Marshal(rel.manifest)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"manifest.json": manifest, rel.manifest.Executable: []byte("binary" + rel.salt),
		"icon.png":                  pngOf(64, 64),
		"locales/en/extension.json": []byte(`{"manifest": {"name": "LichNotes"}}`),
	}
	for tag, body := range rel.locales {
		files["locales/"+tag+"/extension.json"] = []byte(body)
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(files[n])
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sha(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func (f *fixture) build() extension.Catalog {
	f.t.Helper()
	var log bytes.Buffer
	if err := f.b.build(context.Background(), f.dir, &log); err != nil {
		f.t.Fatalf("build: %v\n%s", err, log.String())
	}
	f.t.Log(log.String())
	var cat extension.Catalog
	f.readJSON(catalogFile, &cat)
	return cat
}

func (f *fixture) readJSON(name string, v any) {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		f.t.Fatal(err)
	}
}

// check is `catalog check` on repo: its exit code and what it printed.
func (f *fixture) check(repo string) (int, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), f.b, f.dir, []string{"check", repo}, &out, &errOut)
	return code, out.String() + errOut.String()
}

func onlyEntry(t *testing.T, cat extension.Catalog) extension.CatalogEntry {
	t.Helper()
	if len(cat.Extensions) != 1 {
		t.Fatalf("want one entry, got %+v", cat.Extensions)
	}
	return cat.Extensions[0]
}

func TestBuildListsARelease(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	cat := f.build()
	if _, err := time.Parse(time.RFC3339, cat.Generated); err != nil || cat.FormatVersion != extension.CatalogFormat {
		t.Fatalf("generated %q, format %d", cat.Generated, cat.FormatVersion)
	}
	if len(cat.Categories) != 6 {
		t.Errorf("categories: %+v", cat.Categories)
	}
	e := onlyEntry(t, cat)
	raw := f.gh.srv.URL + "/raw/" + notesRepo + "/" + notesCommit + "/listing/"
	want := extension.CatalogEntry{
		ID: "lich-notes", Name: "LichNotes", Description: notes().manifest.Description,
		Translations: map[string]extension.CatalogText{
			"en":    {Name: "LichNotes"},
			"pt-br": {Name: "LichNotes", Description: "Navegue e edite as notas de uma pasta sua."},
		},
		Publisher: "lichdotdev", Official: true, Stars: 42, Repo: "https://github.com/" + notesRepo,
		RepoID: notesRepoID, License: "AGPL-3.0", Version: "0.2.0", Tag: "v0.2.0", Commit: notesCommit,
		Released: "2026-11-13T10:00:00Z", Categories: []string{"notes"}, Glyph: "notebook-pen",
		Icon:        publishedBase + "icons/lich-notes.png",
		Cover:       raw + "cover.png",
		Screenshots: []extension.CatalogImage{{URL: raw + "screenshots/list.png", Alt: "The list of notes"}},
		Builds:      e.Builds,
	}
	if got, _ := json.Marshal(e); string(got) != string(mustJSON(t, want)) {
		t.Errorf("entry\n got %s\nwant %s", got, mustJSON(t, want))
	}
	if len(e.Builds) != 2 || len(e.Builds["linux_amd64"]) != 64 || len(e.Builds["darwin_arm64"]) != 64 {
		t.Errorf("builds: %v", e.Builds)
	}
	if _, err := os.Stat(filepath.Join(f.dir, iconsDir, "lich-notes.png")); err != nil {
		t.Error(err)
	}
	var lock map[string]lockEntry
	f.readJSON(lockFile, &lock)
	if l := lock["lich-notes@0.2.0"]; l.Commit != notesCommit || len(l.Builds) != 2 {
		t.Errorf("lock: %+v", lock)
	}
	var ids map[string]identity
	f.readJSON(identitiesFile, &ids)
	if ids["lich-notes"] != (identity{Repo: "https://github.com/" + notesRepo, RepoID: notesRepoID}) {
		t.Errorf("identities: %+v", ids)
	}
	if code, out := f.check(notesRepo); code != 0 || !strings.Contains(out, "ok: lich-notes 0.2.0") {
		t.Errorf("check: %d %s", code, out)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAnUnchangedBuildKeepsItsGeneratedTime(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	first := f.build()
	f.write(catalogFile, strings.Replace(string(mustJSON(t, first)), first.Generated, "2020-01-01T00:00:00Z", 1))
	if again := f.build(); again.Generated != "2020-01-01T00:00:00Z" {
		t.Errorf("generated %q after a build that changed nothing", again.Generated)
	}
}

func TestChecksumMismatchIsAProblem(t *testing.T) {
	f := newFixture(t, notesRepo)
	rel := notes()
	rel.sums = map[string]string{"lich-notes_linux_amd64.tar.gz": strings.Repeat("0", 64)}
	f.publish(rel)
	code, out := f.check(notesRepo)
	if code != 1 || !strings.Contains(out, "lich-notes_linux_amd64.tar.gz: checksum mismatch") {
		t.Errorf("check: %d %s", code, out)
	}
	if cat := f.build(); len(cat.Extensions) != 0 {
		t.Errorf("listed with a bad checksum: %+v", cat.Extensions)
	}
}

func TestAReplacedReleaseHoldsTheLockedOne(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	before := onlyEntry(t, f.build())
	rel := notes()
	rel.salt = "replaced"
	f.publish(rel)
	after := onlyEntry(t, f.build())
	if !strings.Contains(after.Held, "is not what the catalog recorded") {
		t.Errorf("held: %q", after.Held)
	}
	after.Held = ""
	if string(mustJSON(t, after)) != string(mustJSON(t, before)) {
		t.Errorf("held entry changed:\n%+v\n%+v", after, before)
	}
	var lock map[string]lockEntry
	f.readJSON(lockFile, &lock)
	if lock["lich-notes@0.2.0"].Builds["linux_amd64"] != before.Builds["linux_amd64"] {
		t.Error("the lock moved")
	}
}

func TestAChangedRepositoryIDHoldsUntilConfirmed(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	f.build()
	rel := notes()
	rel.repoID = 2002
	f.publish(rel)
	e := onlyEntry(t, f.build())
	if e.RepoID != notesRepoID || !strings.Contains(e.Held, "GitHub id 1001") {
		t.Errorf("entry: repoId %d, held %q", e.RepoID, e.Held)
	}
	f.write(identitiesFile, `{"lich-notes": {"repo": "https://github.com/lichdotdev/lich-notes", "repoId": 2002}}`)
	if e := onlyEntry(t, f.build()); e.RepoID != 2002 || e.Held != "" {
		t.Errorf("after confirming: repoId %d, held %q", e.RepoID, e.Held)
	}
}

func TestAMissingReleaseHoldsTheLastGoodEntry(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	f.build()
	f.gh.mu.Lock()
	delete(f.gh.paths, "/api/repos/"+notesRepo+"/releases/latest")
	f.gh.mu.Unlock()
	e := onlyEntry(t, f.build())
	if e.Version != "0.2.0" || !strings.Contains(e.Held, "no published release") {
		t.Errorf("entry: %s held %q", e.Version, e.Held)
	}
	if _, err := os.Stat(filepath.Join(f.dir, iconsDir, "lich-notes.png")); err != nil {
		t.Errorf("held entry lost its icon: %v", err)
	}
}

func TestARevokedRepositoryLeaves(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	f.build()
	f.write(revokedFile, `[{"repo": "https://github.com/lichdotdev/lich-notes", "repoId": 1001, "reason": "malware"}]`)
	cat := f.build()
	if len(cat.Extensions) != 0 || len(cat.Revoked) != 1 || cat.Revoked[0].Reason != "malware" {
		t.Errorf("catalog: %+v", cat)
	}
	if _, err := os.Stat(filepath.Join(f.dir, iconsDir, "lich-notes.png")); !os.IsNotExist(err) {
		t.Errorf("icon of a revoked entry kept: %v", err)
	}
	if code, out := f.check(notesRepo); code != 1 || !strings.Contains(out, "revoked: malware") {
		t.Errorf("check: %d %s", code, out)
	}
}

func TestAManifestWithoutDescriptionIsNotListed(t *testing.T) {
	f := newFixture(t, notesRepo)
	rel := notes()
	rel.manifest.Description = ""
	f.publish(rel)
	if code, out := f.check(notesRepo); code != 1 || !strings.Contains(out, "sets no description") {
		t.Errorf("check: %d %s", code, out)
	}
}

func TestBadListingImagesAreProblems(t *testing.T) {
	f := newFixture(t, notesRepo)
	rel := notes()
	rel.listing = `{"categories": ["notes", "games"], "cover": "cover.png",
		"screenshots": [{"file": "../secret.png", "alt": "x"}, {"file": "small.png", "alt": ""}]}`
	rel.images = map[string][]byte{"cover.png": pngOf(1600, 1200), "small.png": pngOf(640, 400)}
	f.publish(rel)
	code, out := f.check(notesRepo)
	for _, want := range []string{
		`"games" is not a category`,
		"listing/cover.png: 1600 × 1200 does not fit a cover",
		`screenshot "../secret.png" must be a path inside listing/`,
		"small.png: alt is required",
		"listing/small.png: 640 × 400 does not fit a screenshot",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check does not say %q:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("exit %d", code)
	}
}

func TestNoListingIsTools(t *testing.T) {
	f := newFixture(t, notesRepo)
	rel := notes()
	rel.listing, rel.images = "", nil
	f.publish(rel)
	code, out := f.check(notesRepo)
	if code != 0 || !strings.Contains(out, "warning: no listing/listing.json") {
		t.Errorf("check: %d %s", code, out)
	}
	if e := onlyEntry(t, f.build()); len(e.Categories) != 1 || e.Categories[0] != "tools" || e.Cover != "" {
		t.Errorf("entry: %+v", e)
	}
}

func TestAMissingPlatformIsListedWithoutIt(t *testing.T) {
	f := newFixture(t, notesRepo)
	rel := notes()
	rel.platforms = []string{"linux_amd64"}
	f.publish(rel)
	e := onlyEntry(t, f.build())
	if len(e.Builds) != 1 || e.Builds["linux_amd64"] == "" || e.Held != "" {
		t.Errorf("builds %v, held %q", e.Builds, e.Held)
	}
}

func TestAFailedLichCheckIsAProblem(t *testing.T) {
	f := newFixture(t, notesRepo)
	f.publish(notes())
	t.Setenv(fakeLichFail, "lich-notes_darwin_arm64.tar.gz")
	code, out := f.check(notesRepo)
	if code != 1 || !strings.Contains(out, "lich-notes_darwin_arm64.tar.gz: lich extension check: manifest.json: executable is missing") {
		t.Errorf("check: %d %s", code, out)
	}
}

func TestALichIDIsKeptForLichdotdev(t *testing.T) {
	f := newFixture(t, "someone/lich-tools")
	rel := notes()
	rel.repo, rel.repoID = "someone/lich-tools", 3003
	rel.manifest.ID = "lichtools"
	f.publish(rel)
	code, out := f.check("someone/lich-tools")
	if code != 1 || !strings.Contains(out, `the id "lichtools" starts with "lich", which is kept for lichdotdev`) {
		t.Errorf("check: %d %s", code, out)
	}
}

func TestAnIDIsListedFromOneRepository(t *testing.T) {
	f := newFixture(t, notesRepo, "someone/notes")
	f.publish(notes())
	copycat := notes()
	copycat.repo, copycat.repoID = "someone/notes", 4004
	f.publish(copycat)
	e := onlyEntry(t, f.build())
	if e.Repo != "https://github.com/"+notesRepo {
		t.Errorf("listed from %s", e.Repo)
	}
	if code, out := f.check("someone/notes"); code != 1 || !strings.Contains(out, `the id "lich-notes" was listed from`) {
		t.Errorf("check: %d %s", code, out)
	}
}
