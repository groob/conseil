package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type githubMinter struct {
	appID        int64
	organization string
	privateKey   *rsa.PrivateKey
	client       *http.Client
}

func newGitHubMinter(appID int64, organization string, privateKey *rsa.PrivateKey, client *http.Client) *githubMinter {
	return &githubMinter{
		appID:        appID,
		organization: organization,
		privateKey:   privateKey,
		client:       client,
	}
}

func (m *githubMinter) mint(ctx context.Context, repository string) (installationToken, error) {
	appToken, err := m.signAppToken()
	if err != nil {
		return installationToken{}, fmt.Errorf("signing GitHub App token: %w", err)
	}

	var installation struct {
		ID int64 `json:"id"`
	}
	installationPath := "/orgs/" + url.PathEscape(m.organization) + "/installation"
	if err := m.request(ctx, http.MethodGet, installationPath, appToken, nil, &installation); err != nil {
		return installationToken{}, fmt.Errorf("finding GitHub App installation: %w", err)
	}
	if installation.ID <= 0 {
		return installationToken{}, errors.New("finding GitHub App installation: GitHub returned an invalid installation ID")
	}

	requestBody, err := json.Marshal(struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{
		Repositories: []string{repository},
		Permissions: map[string]string{
			"contents":      "write",
			"pull_requests": "write",
		},
	})
	if err != nil {
		return installationToken{}, fmt.Errorf("encoding installation token request: %w", err)
	}
	var response struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID)
	if err := m.request(ctx, http.MethodPost, path, appToken, bytes.NewReader(requestBody), &response); err != nil {
		return installationToken{}, fmt.Errorf("creating GitHub installation token: %w", err)
	}
	if response.Token == "" || response.ExpiresAt.IsZero() {
		return installationToken{}, errors.New("creating GitHub installation token: GitHub returned an incomplete token")
	}
	if err := validateInstallationPermissions(response.Permissions); err != nil {
		return installationToken{}, fmt.Errorf("creating GitHub installation token: %w", err)
	}
	return installationToken{Token: response.Token, ExpiresAt: response.ExpiresAt}, nil
}

func validateInstallationPermissions(permissions map[string]string) error {
	want := map[string]string{
		"contents":      "write",
		"metadata":      "read",
		"pull_requests": "write",
	}
	for permission, access := range want {
		if permissions[permission] != access {
			return fmt.Errorf("%s permission is not %s", permission, access)
		}
	}
	for permission, access := range permissions {
		if access == "write" && want[permission] != "write" {
			return fmt.Errorf("unexpected write permission %s", permission)
		}
	}
	return nil
}

func (m *githubMinter) signAppToken() (string, error) {
	now := time.Now()
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("encoding header: %w", err)
	}
	claims, err := json.Marshal(struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}{
		IssuedAt:  now.Add(-time.Minute).Unix(),
		ExpiresAt: now.Add(9 * time.Minute).Unix(),
		Issuer:    m.appID,
	})
	if err != nil {
		return "", fmt.Errorf("encoding claims: %w", err)
	}

	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, m.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing claims: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (m *githubMinter) request(ctx context.Context, method, path, appToken string, body io.Reader, destination any) error {
	request, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+appToken)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "vitrier-token-broker")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 8192))
		if readErr != nil {
			return fmt.Errorf("reading GitHub error response with status %s: %w", response.Status, readErr)
		}
		return fmt.Errorf("GitHub returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decoding response: trailing JSON value or data")
	}
	return nil
}

func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("private key is not PEM data")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}
