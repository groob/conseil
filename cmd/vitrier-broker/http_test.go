package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenEndpointRejectsRequestsWithoutGrant(t *testing.T) {
	t.Parallel()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var githubRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		githubRequests.Add(1)
	}))
	defer server.Close()
	api := &api{
		grants: newGrantStore(filepath.Join(t.TempDir(), "missing-grants")),
		minter: newGitHubMinter(4691351, "maintenancewindows", privateKey, githubTestClient(t, server)),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	for _, sourceVM := range []string{"", "other-dev"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/token", nil)
		request.Header.Set("X-Exedev-Source-Vm", sourceVM)
		response := httptest.NewRecorder()
		api.handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("source VM %q status = %d, want %d", sourceVM, response.Code, http.StatusForbidden)
		}
	}
	if got := githubRequests.Load(); got != 0 {
		t.Errorf("GitHub request count = %d, want 0", got)
	}
}

func TestTokenEndpointMapsPeerToRepository(t *testing.T) {
	requireRoot(t)

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/orgs/maintenancewindows/installation":
			_, _ = response.Write([]byte(`{"id":123}`))
		case "/app/installations/123/access_tokens":
			var body struct {
				Repositories []string `json:"repositories"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Repositories) != 1 {
				t.Fatalf("repositories = %v, want one", body.Repositories)
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{
				"token":      "token-" + body.Repositories[0],
				"expires_at": time.Now().Add(time.Hour).UTC(),
				"permissions": map[string]string{
					"contents": "write", "metadata": "read", "pull_requests": "write",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	store := newGrantStore(filepath.Join(t.TempDir(), "grants"))
	grants := map[string]string{"samovar-dev": "samovar", "conseil-dev": "conseil"}
	for vm, repository := range grants {
		if _, err := store.grant(vm, repository); err != nil {
			t.Fatalf("grant(%q, %q): %v", vm, repository, err)
		}
	}
	api := &api{
		grants: store,
		minter: newGitHubMinter(4691351, "maintenancewindows", privateKey, githubTestClient(t, server)),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for sourceVM, repository := range grants {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/token?repository=worker-choice", nil)
		request.Header.Set("X-Exedev-Source-Vm", sourceVM)
		response := httptest.NewRecorder()
		api.handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("source VM %q status = %d, want %d", sourceVM, response.Code, http.StatusOK)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		var token installationToken
		if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
			t.Fatal(err)
		}
		if token.Token != "token-"+repository {
			t.Errorf("source VM %q token = %q, want %q", sourceVM, token.Token, "token-"+repository)
		}
	}
}
