package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// A problem with the repository itself, which holds or refuses its listing.
// Any other error (a timeout, a 5xx, a rate limit) stops the run: the next one
// retries, and no entry is held for GitHub's fault.
var (
	errNotFound = errors.New("not found")
	errTooLarge = errors.New("too large")
)

const (
	maxJSONBytes = 4 << 20
	// docs/extensions.md, Publishing a release.
	maxArchiveBytes = 256 << 20
)

type ghRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private  bool `json:"private"`
	Fork     bool `json:"fork"`
	Archived bool `json:"archived"`
	Stars    int  `json:"stargazers_count"`
	License  *struct {
		SPDX string `json:"spdx_id"`
	} `json:"license"`
}

type ghRelease struct {
	Tag       string    `json:"tag_name"`
	Published string    `json:"published_at"`
	Assets    []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func (b *builder) request(ctx context.Context, u string, api bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// The token goes to the API only, never to a download host.
	if api && b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %w", u, errNotFound)
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
}

func (b *builder) fetch(ctx context.Context, u string, api bool, limit int64) ([]byte, error) {
	resp, err := b.request(ctx, u, api)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: over %d bytes: %w", u, limit, errTooLarge)
	}
	return body, nil
}

func (b *builder) apiJSON(ctx context.Context, path string, v any) error {
	body, err := b.fetch(ctx, b.api+path, true, maxJSONBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (b *builder) repo(ctx context.Context, full string) (ghRepo, error) {
	var r ghRepo
	return r, b.apiJSON(ctx, "/repos/"+full, &r)
}

// latestRelease is GitHub's latest, the newest that is neither a draft nor a
// pre-release: the one lich installs.
func (b *builder) latestRelease(ctx context.Context, full string) (ghRelease, error) {
	var r ghRelease
	return r, b.apiJSON(ctx, "/repos/"+full+"/releases/latest", &r)
}

func (b *builder) commit(ctx context.Context, full, tag string) (string, error) {
	var c struct {
		SHA string `json:"sha"`
	}
	return c.SHA, b.apiJSON(ctx, "/repos/"+full+"/commits/"+url.PathEscape(tag), &c)
}

// raw is a file of the repository at commit, from raw.githubusercontent.com.
func (b *builder) rawURL(full, commit, file string) string {
	segments := strings.Split(file, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return b.raw + "/" + full + "/" + commit + "/" + strings.Join(segments, "/")
}

// download writes u to dst and returns its sha256.
func (b *builder) download(ctx context.Context, u, dst string) (string, error) {
	resp, err := b.request(ctx, u, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("%s: over %d bytes: %w", u, maxArchiveBytes, errTooLarge)
	}
	return hex.EncodeToString(h.Sum(nil)), f.Close()
}

// repoProblem is true for an error that is the repository's to fix.
func repoProblem(err error) bool {
	return errors.Is(err, errNotFound) || errors.Is(err, errTooLarge)
}
