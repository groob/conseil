package devbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultArtifactDir returns the default host directory for captured task artifacts.
func DefaultArtifactDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(".", "devbox-artifacts")
	}
	return filepath.Join(dir, "conseil", "devbox-artifacts")
}

func (f *Factory) captureTaskArtifacts(ctx context.Context, session Session, expected taskResult, artifactDir string) (piResult, error) {
	result := piResult{Evidence: expected.Evidence}
	dir, err := prepareArtifactDir(artifactDir, session.ID)
	if err != nil {
		return result, err
	}
	defer func() { _ = dir.root.Close() }()
	outputPath, outputSHA256, outputErr := f.copyTaskArtifact(ctx, session, dir, taskOutputArtifact, expected.OutputSHA256)
	if outputErr == nil {
		result.OutputPath = outputPath
		result.OutputSHA256 = outputSHA256
	}
	sessionPath, sessionSHA256, sessionErr := f.copyTaskArtifact(ctx, session, dir, taskSessionArtifact, expected.SessionSHA256)
	if sessionErr == nil {
		result.SessionPath = sessionPath
		result.SessionSHA256 = sessionSHA256
	}
	if outputErr != nil || sessionErr != nil {
		return result, errors.Join(outputErr, sessionErr)
	}
	return result, nil
}

type hostArtifactDir struct {
	root *os.Root
	path string
}

func (f *Factory) copyTaskArtifact(ctx context.Context, session Session, dir hostArtifactDir, artifact, expectedSHA256 string) (string, string, error) {
	destination := filepath.Join(dir.path, artifact)
	if info, err := dir.root.Lstat(artifact); err == nil {
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("host artifact %s is not a regular file", artifact)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("inspecting host artifact %s: %w", artifact, err)
	}
	temporary, temporaryName, err := createArtifactTemp(dir.root, artifact)
	if err != nil {
		return "", "", fmt.Errorf("creating host artifact %s: %w", artifact, err)
	}
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = dir.root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", "", fmt.Errorf("setting host artifact %s mode: %w", artifact, err)
	}
	hash := sha256.New()
	writer := &boundedArtifactWriter{Writer: io.MultiWriter(temporary, hash), Remaining: maxPiArtifactSize}
	remote := "exec /usr/local/libexec/conseil-devbox stream-artifact --session " + session.ID + " --artifact " + artifact
	if err := runControlCommand(ctx, command{Name: "ssh", Args: []string{session.SSHDest, remote}, Stdout: writer, Stderr: f.logs}); err != nil {
		return "", "", fmt.Errorf("streaming remote artifact %s: %w", artifact, err)
	}
	if writer.Exceeded {
		return "", "", fmt.Errorf("remote artifact %s exceeds %d bytes", artifact, maxPiArtifactSize)
	}
	info, err := temporary.Stat()
	if err != nil {
		return "", "", fmt.Errorf("inspecting host artifact %s: %w", artifact, err)
	}
	if info.Size() == 0 {
		return "", "", fmt.Errorf("remote artifact %s is empty", artifact)
	}
	gotSHA256 := hex.EncodeToString(hash.Sum(nil))
	if expectedSHA256 != "" && gotSHA256 != expectedSHA256 {
		return "", "", fmt.Errorf("remote artifact %s SHA-256 mismatch: got %s want %s", artifact, gotSHA256, expectedSHA256)
	}
	if err := temporary.Sync(); err != nil {
		return "", "", fmt.Errorf("syncing host artifact %s: %w", artifact, err)
	}
	if err := temporary.Close(); err != nil {
		return "", "", fmt.Errorf("closing host artifact %s: %w", artifact, err)
	}
	if err := dir.root.Rename(temporaryName, artifact); err != nil {
		return "", "", fmt.Errorf("installing host artifact %s: %w", artifact, err)
	}
	keep = true
	directory, err := dir.root.Open(".")
	if err != nil {
		return "", "", fmt.Errorf("opening host artifact directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return "", "", fmt.Errorf("syncing host artifact directory: %w", syncErr)
	}
	if closeErr != nil {
		return "", "", fmt.Errorf("closing host artifact directory: %w", closeErr)
	}
	return destination, gotSHA256, nil
}

type boundedArtifactWriter struct {
	Writer    io.Writer
	Remaining int64
	Exceeded  bool
}

func (w *boundedArtifactWriter) Write(contents []byte) (int, error) {
	if int64(len(contents)) > w.Remaining {
		w.Exceeded = true
		return 0, fmt.Errorf("pi artifact exceeds %d bytes", maxPiArtifactSize)
	}
	n, err := w.Writer.Write(contents)
	w.Remaining -= int64(n)
	return n, err
}

func createArtifactTemp(root *os.Root, artifact string) (*os.File, string, error) {
	for range 100 {
		name := "." + artifact + "-" + rand.Text() + ".tmp"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate artifact temporary file")
}

func prepareArtifactDir(root, session string) (hostArtifactDir, error) {
	if err := validateSessionID(session); err != nil {
		return hostArtifactDir{}, err
	}
	if strings.TrimSpace(root) == "" {
		return hostArtifactDir{}, errors.New("artifact directory is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return hostArtifactDir{}, fmt.Errorf("resolving artifact directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return hostArtifactDir{}, fmt.Errorf("creating artifact directory: %w", err)
	}
	if err := requirePrivateDirectory(absolute); err != nil {
		return hostArtifactDir{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return hostArtifactDir{}, fmt.Errorf("resolving artifact directory symlinks: %w", err)
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return hostArtifactDir{}, errors.New("artifact directory changed while resolving")
	}
	rootDir, err := os.OpenRoot(resolved)
	if err != nil {
		return hostArtifactDir{}, fmt.Errorf("opening artifact directory: %w", err)
	}
	openedRootInfo, err := rootDir.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedRootInfo) {
		_ = rootDir.Close()
		return hostArtifactDir{}, errors.New("artifact directory changed while opening")
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			_ = rootDir.Close()
		}
	}()
	if err := rootDir.Mkdir(session, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return hostArtifactDir{}, fmt.Errorf("creating session artifact directory: %w", err)
	}
	info, err := rootDir.Lstat(session)
	if err != nil {
		return hostArtifactDir{}, fmt.Errorf("inspecting session artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return hostArtifactDir{}, errors.New("session artifact path is not a secure directory")
	}
	sessionRoot, err := rootDir.OpenRoot(session)
	if err != nil {
		return hostArtifactDir{}, fmt.Errorf("opening session artifact directory: %w", err)
	}
	openedInfo, err := sessionRoot.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = sessionRoot.Close()
		return hostArtifactDir{}, errors.New("session artifact directory changed while opening")
	}
	closeRoot = false
	_ = rootDir.Close()
	return hostArtifactDir{root: sessionRoot, path: filepath.Join(resolved, session)}, nil
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspecting artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact path %s is not a directory", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("artifact directory %s is writable by other users", path)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
