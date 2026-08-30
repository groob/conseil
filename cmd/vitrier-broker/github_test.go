package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type githubRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
	t      *testing.T
}

func (transport githubRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Helper()
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" {
		transport.t.Errorf("GitHub request URL = %s, want https://api.github.com", request.URL)
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme = transport.target.Scheme
	clone.URL.Host = transport.target.Host
	clone.Host = transport.target.Host
	return transport.base.RoundTrip(clone)
}

func githubTestClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: githubRewriteTransport{target: target, base: server.Client().Transport, t: t}}
}

type appClaims struct {
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
	Issuer    int64 `json:"iss"`
}

func TestGitHubMinterRequestsRepositoryScopedWriteToken(t *testing.T) {
	t.Parallel()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requestCount atomic.Int32
	var claimsMu sync.Mutex
	var allClaims []appClaims
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		claims := verifyAppToken(t, request.Header.Get("Authorization"), &privateKey.PublicKey, 4691351)
		claimsMu.Lock()
		allClaims = append(allClaims, claims)
		claimsMu.Unlock()
		if request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", request.Header.Get("X-GitHub-Api-Version"))
		}

		switch request.URL.Path {
		case "/orgs/maintenancewindows/installation":
			if request.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", request.Method)
			}
			_, _ = response.Write([]byte(`{"id":123,"future_field":true}`))
		case "/app/installations/123/access_tokens":
			if request.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", request.Method)
			}
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Repositories) != 1 || body.Repositories[0] != "samovar" {
				t.Errorf("repositories = %v, want [samovar]", body.Repositories)
			}
			if len(body.Permissions) != 2 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
				t.Errorf("permissions = %v", body.Permissions)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"token":"installation-token","expires_at":"2026-08-29T13:00:00Z","permissions":{"contents":"write","metadata":"read","pull_requests":"write"},"future_field":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	minter := newGitHubMinter(4691351, "maintenancewindows", privateKey, githubTestClient(t, server))
	beforeMint := time.Now()
	token, err := minter.mint(t.Context(), "samovar")
	afterMint := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "installation-token" {
		t.Errorf("token = %q, want installation-token", token.Token)
	}
	if want := time.Date(2026, time.August, 29, 13, 0, 0, 0, time.UTC); !token.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %s, want %s", token.ExpiresAt, want)
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("request count = %d, want 2", got)
	}
	claimsMu.Lock()
	defer claimsMu.Unlock()
	for _, claims := range allClaims {
		if claims.ExpiresAt-claims.IssuedAt != int64((10*time.Minute)/time.Second) {
			t.Errorf("JWT lifetime = %d seconds, want 600", claims.ExpiresAt-claims.IssuedAt)
		}
		if claims.IssuedAt < beforeMint.Add(-time.Minute).Unix() || claims.IssuedAt > afterMint.Add(-time.Minute).Unix() {
			t.Errorf("JWT issued at = %d, want between %d and %d", claims.IssuedAt, beforeMint.Add(-time.Minute).Unix(), afterMint.Add(-time.Minute).Unix())
		}
	}
}

func TestGitHubMinterRejectsTrailingResponseData(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"second_JSON_value": `{"id":123}{"id":456}`,
		"trailing_text":     `{"id":123} trailing`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = response.Write([]byte(body))
			}))
			defer server.Close()
			minter := newGitHubMinter(4691351, "maintenancewindows", privateKey, githubTestClient(t, server))
			if _, err := minter.mint(t.Context(), "samovar"); err == nil {
				t.Error("mint accepted trailing response data")
			}
		})
	}
}

func TestValidateInstallationPermissions(t *testing.T) {
	tests := []struct {
		name        string
		permissions map[string]string
		wantError   bool
	}{
		{name: "required_permissions", permissions: map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}},
		{name: "missing_metadata", permissions: map[string]string{"contents": "write", "pull_requests": "write"}, wantError: true},
		{name: "downgraded_contents", permissions: map[string]string{"contents": "read", "metadata": "read", "pull_requests": "write"}, wantError: true},
		{name: "unexpected_write", permissions: map[string]string{"contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateInstallationPermissions(test.permissions)
			if gotError := err != nil; gotError != test.wantError {
				t.Errorf("validateInstallationPermissions(%v) error = %v, want error presence = %t", test.permissions, err, test.wantError)
			}
		})
	}
}

func verifyAppToken(t *testing.T, authorization string, publicKey *rsa.PublicKey, appID int64) appClaims {
	t.Helper()
	const prefix = "Bearer "
	token, ok := strings.CutPrefix(authorization, prefix)
	if !ok {
		t.Fatalf("Authorization = %q", authorization)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verifying JWT: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims appClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != appID {
		t.Errorf("issuer = %d, want %d", claims.Issuer, appID)
	}
	return claims
}
