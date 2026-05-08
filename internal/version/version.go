// Package version provides a background GitHub release checker with 24-hour caching.
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version is the current binary version, injected at build time via
// -ldflags="-X github.com/pi-agent/pi/internal/version.Version=x.y.z"
var Version = "dev"

const (
	releasesURL = "https://api.github.com/repos/pi-agent/pi/releases/latest"
	cacheTTL    = 24 * time.Hour
	httpTimeout = 10 * time.Second
)

// Release holds information about a GitHub release.
type Release struct {
	Version     string
	PublishedAt time.Time
	URL         string
}

// versionCache is the on-disk cache format.
type versionCache struct {
	FetchedAt   time.Time `json:"fetched_at"`
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url"`
}

// cachePath returns the path to the version cache file.
func cachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", ".version-cache.json")
}

// Check fetches the latest GitHub release, using a 24-hour cache.
// It returns nil if the release info cannot be determined.
func Check(ctx context.Context) (*Release, error) {
	// Try the cache first.
	if cached, ok := readCache(); ok {
		return cached, nil
	}

	return fetch(ctx)
}

// IsNewer returns true if release.Version is strictly newer than the current Version.
func IsNewer(release *Release) bool {
	if release == nil {
		return false
	}
	curr := parseSemver(Version)
	latest := parseSemver(release.Version)
	if curr == nil || latest == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if latest[i] > curr[i] {
			return true
		}
		if latest[i] < curr[i] {
			return false
		}
	}
	return false
}

// parseSemver parses a version string like "v1.2.3" or "1.2.3" into [major, minor, patch].
// Returns nil if the string cannot be parsed.
func parseSemver(v string) []int {
	v = strings.TrimPrefix(v, "v")
	// Strip pre-release/build suffixes.
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		// Try padding with zeros for e.g. "1.2"
		for len(parts) < 3 {
			parts = append(parts, "0")
		}
		parts = parts[:3]
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		nums[i] = n
	}
	return nums
}

// readCache reads the cached release if it is still fresh (within TTL).
func readCache() (*Release, bool) {
	data, err := os.ReadFile(cachePath())
	if err != nil {
		return nil, false
	}
	var c versionCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	if time.Since(c.FetchedAt) > cacheTTL {
		return nil, false
	}
	if c.Version == "" {
		return nil, false
	}
	return &Release{
		Version:     c.Version,
		PublishedAt: c.PublishedAt,
		URL:         c.URL,
	}, true
}

// writeCache persists the release to the cache file.
func writeCache(r *Release) {
	c := versionCache{
		FetchedAt:   time.Now().UTC(),
		Version:     r.Version,
		PublishedAt: r.PublishedAt,
		URL:         r.URL,
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	path := cachePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, data, 0o600)
}

// fetch queries the GitHub releases API.
func fetch(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("version: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pi-agent/"+Version)

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("version: request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("version: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("version: GitHub API returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("version: parse response: %w", err)
	}
	if payload.TagName == "" {
		return nil, fmt.Errorf("version: response missing tag_name")
	}

	release := &Release{
		Version:     strings.TrimPrefix(payload.TagName, "v"),
		PublishedAt: payload.PublishedAt,
		URL:         payload.HTMLURL,
	}
	writeCache(release)
	return release, nil
}
