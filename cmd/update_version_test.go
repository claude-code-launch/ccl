package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withReleaseAndNpm points both version lookups at local servers. A status of
// 0 makes the endpoint fail, standing for an unreachable service.
func withReleaseAndNpm(t *testing.T, releaseStatus int, releaseBody string, npmStatus int, npmBody string) {
	t.Helper()
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if releaseStatus == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(releaseStatus)
		_, _ = w.Write([]byte(releaseBody))
	}))
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if npmStatus == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(npmStatus)
		_, _ = w.Write([]byte(npmBody))
	}))
	t.Cleanup(release.Close)
	t.Cleanup(npm.Close)

	previousAPI, previousClient := latestReleaseAPI, updateHTTPClient
	latestReleaseAPI = release.URL
	updateHTTPClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripTo(map[string]string{
			"https://registry.npmjs.org/": npm.URL + "/",
		}),
	}
	t.Cleanup(func() { latestReleaseAPI, updateHTTPClient = previousAPI, previousClient })
}

// roundTripTo rewrites requests for the given hosts to a local server.
func roundTripTo(routes map[string]string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		original := req.URL.Scheme + "://" + req.URL.Host + "/"
		if target, ok := routes[original]; ok {
			rewritten := *req.URL
			rewritten.Scheme = "http"
			rewritten.Host = strings.TrimPrefix(strings.TrimSuffix(target, "/"), "http://")
			req = req.Clone(req.Context())
			req.URL = &rewritten
		}
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestLatestVersionsPrefersTheRelease(t *testing.T) {
	// The GitHub release exists before the npm job has published: the release
	// version is what update installs, and npm is not offered yet.
	withReleaseAndNpm(t, http.StatusOK, `{"tag_name":"v1.6.6"}`, http.StatusOK, `{"version":"1.6.5"}`)
	latest, npmReady := latestVersions()
	if latest != "v1.6.6" || npmReady {
		t.Fatalf("latestVersions() = %q, %v; want v1.6.6, false", latest, npmReady)
	}

	withReleaseAndNpm(t, http.StatusOK, `{"tag_name":"v1.6.6"}`, http.StatusOK, `{"version":"1.6.6"}`)
	if latest, npmReady = latestVersions(); latest != "v1.6.6" || !npmReady {
		t.Fatalf("latestVersions() = %q, %v; want v1.6.6, true", latest, npmReady)
	}
}

func TestLatestVersionsFallsBackToNpm(t *testing.T) {
	// The GitHub API is blocked on some networks; npm still answers.
	withReleaseAndNpm(t, http.StatusForbidden, ``, http.StatusOK, `{"version":"1.6.5"}`)
	latest, npmReady := latestVersions()
	if latest != "v1.6.5" || !npmReady {
		t.Fatalf("latestVersions() = %q, %v; want v1.6.5, true", latest, npmReady)
	}

	withReleaseAndNpm(t, http.StatusForbidden, ``, http.StatusNotFound, `{}`)
	if latest, npmReady = latestVersions(); latest != "unknown" || npmReady {
		t.Fatalf("latestVersions() = %q, %v; want unknown, false", latest, npmReady)
	}
}

// A draft release is not something to install.
func TestLatestVersionsRejectsUnstableRelease(t *testing.T) {
	withReleaseAndNpm(t, http.StatusOK, `{"tag_name":"v1.6.6","draft":true}`, http.StatusOK, `{"version":"1.6.5"}`)
	if latest, _ := latestVersions(); latest != "v1.6.5" {
		t.Fatalf("draft release was used: %q", latest)
	}
	withReleaseAndNpm(t, http.StatusOK, `{"tag_name":"v1.6.6","prerelease":true}`, http.StatusOK, `{"version":"1.6.6"}`)
	if latest, _ := latestVersions(); latest != "v1.6.6" {
		t.Fatalf("prerelease was used: %q", latest)
	}
}

func TestUpdateChoicesDropNpmUntilPublished(t *testing.T) {
	without := updateChoices(false)
	if len(without) != 2 || without[0].method != "self" || without[1].method != "go" {
		t.Fatalf("choices without npm = %+v", without)
	}
	with := updateChoices(true)
	if len(with) != 3 || with[1].method != "npm" {
		t.Fatalf("choices with npm = %+v", with)
	}
}
