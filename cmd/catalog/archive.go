package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/lichdotdev/lich/extension"
)

const checksumsAsset = "checksums.txt"

// maxKeptFile is the largest archive entry read into memory: a manifest, a
// catalog or an icon is far below it, and the executable is never needed.
const maxKeptFile = 8 << 20

// archivePattern is <id>_<goos>_<goarch>.tar.gz, the id under the manifest's
// own rules so it never holds a path separator.
var archivePattern = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]{0,63})_([a-z0-9]+)_([a-z0-9]+)\.tar\.gz$`)

type archive struct {
	name     string
	platform string
	sha      string
	manifest extension.Manifest
	files    fstest.MapFS
}

// archives downloads checksums.txt and every archive of rel, and checks each
// against its checksum and `lich extension check`. Only archives that pass
// are returned; what fails is a problem on r.
func (b *builder) archives(ctx context.Context, rel ghRelease, r *report) ([]archive, error) {
	sums, err := b.checksums(ctx, rel)
	if err != nil {
		if !repoProblem(err) {
			return nil, err
		}
		r.problem("%s: %v", checksumsAsset, err)
		return nil, nil
	}
	tmp, err := os.MkdirTemp("", "catalog-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	carried := map[string]bool{}
	var out []archive
	for _, asset := range rel.Assets {
		m := archivePattern.FindStringSubmatch(asset.Name)
		if m == nil {
			continue
		}
		carried[asset.Name] = true
		a, err := b.archive(ctx, asset, filepath.Join(tmp, asset.Name), sums[asset.Name], r)
		if err != nil {
			return nil, err
		}
		if a != nil {
			a.platform = m[2] + "_" + m[3]
			out = append(out, *a)
		}
	}
	for name := range sums {
		if !carried[name] {
			r.problem("%s names %s, which the release does not carry", checksumsAsset, name)
		}
	}
	if len(carried) == 0 {
		r.problem("the release carries no <id>_<goos>_<goarch>.tar.gz archive")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].platform < out[j].platform })
	return out, nil
}

func (b *builder) archive(ctx context.Context, asset ghAsset, dst, want string, r *report) (*archive, error) {
	sha, err := b.download(ctx, asset.URL, dst)
	if err != nil {
		if !repoProblem(err) {
			return nil, err
		}
		r.problem("%s: %v", asset.Name, err)
		return nil, nil
	}
	ok := true
	switch {
	case want == "":
		r.problem("%s is not in %s", asset.Name, checksumsAsset)
		ok = false
	case want != sha:
		r.problem("%s: checksum mismatch: %s says %s, the archive is %s", asset.Name, checksumsAsset, want, sha)
		ok = false
	}
	if out, err := exec.CommandContext(ctx, b.lich, "extension", "check", dst).CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("run %s: %w", b.lich, err)
		}
		r.problem("%s: lich extension check: %s", asset.Name, strings.TrimSpace(string(out)))
		ok = false
	}
	files, err := readArchive(dst)
	if err != nil {
		r.problem("%s: %v", asset.Name, err)
		return nil, nil
	}
	var m extension.Manifest
	mf, found := files[extension.ManifestFile]
	if !found {
		r.problem("%s: no %s at its root", asset.Name, extension.ManifestFile)
		return nil, nil
	}
	if err := json.Unmarshal(mf.Data, &m); err != nil {
		r.problem("%s: %s: %v", asset.Name, extension.ManifestFile, err)
		return nil, nil
	}
	if !ok {
		return nil, nil
	}
	return &archive{name: asset.Name, sha: sha, manifest: m, files: files}, nil
}

func (b *builder) checksums(ctx context.Context, rel ghRelease) (map[string]string, error) {
	for _, asset := range rel.Assets {
		if asset.Name != checksumsAsset {
			continue
		}
		body, err := b.fetch(ctx, asset.URL, false, maxJSONBytes)
		if err != nil {
			return nil, err
		}
		return parseChecksums(body), nil
	}
	return nil, fmt.Errorf("the release carries none: %w", errNotFound)
}

// parseChecksums reads sha256sum's output, text or binary mode.
func parseChecksums(body []byte) map[string]string {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return sums
}

// readArchive is every small regular file of a .tar.gz, in memory, so nothing
// an archive names is ever written to disk.
func readArchive(file string) (fstest.MapFS, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	files := fstest.MapFS{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if h.Typeflag != tar.TypeReg || h.Size > maxKeptFile || !fs.ValidPath(name) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[name] = &fstest.MapFile{Data: data}
	}
}
