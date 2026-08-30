package devbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxTaskRequest = 2 << 20
const maxPiOutputLine = 64 << 20
const maxPiArtifactSize int64 = 1 << 30

const (
	taskOutputArtifact  = "events.jsonl"
	taskSessionArtifact = "session.jsonl"
)

const (
	piProvider = "exe-dev-openai"
	piModel    = "gpt-5.6-sol@llm"
	piThinking = "high"
)

type taskRequest struct {
	Task        string            `json:"task"`
	Environment map[string]string `json:"environment"`
}

func runTask(ctx context.Context, session, workspace string, stdin io.Reader, stderr, resultOut io.Writer) error {
	if err := validateSessionID(session); err != nil {
		return err
	}
	if strings.TrimSpace(workspace) == "" {
		return errors.New("task workspace is empty")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home: %w", err)
	}
	request, err := decodeTaskRequest(stdin)
	if err != nil {
		return err
	}
	if after, ok := strings.CutPrefix(workspace, "$HOME/"); ok {
		workspace = filepath.Join(home, after)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return fmt.Errorf("resolving task workspace: %w", err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return fmt.Errorf("task workspace is not a directory: %s", workspace)
	}

	root, err := createTaskArtifactRoot(home, session)
	if err != nil {
		return err
	}
	sessionDir := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		return fmt.Errorf("creating Pi session directory: %w", err)
	}
	outputPath := filepath.Join(root, taskOutputArtifact)
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating Pi output: %w", err)
	}

	env := mergeEnvironment(os.Environ(), request.Environment)
	env = mergeEnvironment(env, map[string]string{
		"HOME":                        home,
		"PI_CODING_AGENT_SESSION_DIR": sessionDir,
	})
	env = prependPath(env, filepath.Join(home, ".local", "bin"))
	command := command{
		Name:   "pi",
		Args:   []string{"--mode", "json"},
		Env:    env,
		Dir:    workspace,
		Stdin:  strings.NewReader(request.Task),
		Stdout: output,
		Stderr: stderr,
	}
	runErr := osCommand(ctx, command)
	closeErr := output.Close()
	sessionSource, findErr := findPiSession(sessionDir)
	artifactRoot, artifactRootPath, artifactRootErr := openTaskDirectory(home, false, ".local", "share", "conseil", "pi", session)
	if artifactRootErr == nil {
		defer func() { _ = artifactRoot.Close() }()
	}
	sessionPath := filepath.Join(artifactRootPath, taskSessionArtifact)
	var installErr error
	if findErr == nil && artifactRootErr == nil {
		installErr = installTaskSession(sessionSource, artifactRoot, taskSessionArtifact)
	}
	if runErr != nil {
		return fmt.Errorf("running plain Pi: %w", runErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing Pi output: %w", closeErr)
	}
	if findErr != nil {
		return findErr
	}
	if artifactRootErr != nil {
		return fmt.Errorf("opening fixed Pi artifact directory: %w", artifactRootErr)
	}
	if installErr != nil {
		return fmt.Errorf("installing fixed Pi session artifact: %w", installErr)
	}
	if err := verifyPiOutput(outputPath); err != nil {
		return fmt.Errorf("verifying Pi JSON output: %w", err)
	}
	evidence, err := verifyPiSession(sessionPath)
	if err != nil {
		return fmt.Errorf("verifying Pi session: %w", err)
	}
	sessionSHA256, err := hashTaskArtifact(home, session, taskSessionArtifact)
	if err != nil {
		return fmt.Errorf("hashing Pi session artifact: %w", err)
	}
	outputSHA256, err := hashTaskArtifact(home, session, taskOutputArtifact)
	if err != nil {
		return fmt.Errorf("hashing Pi output artifact: %w", err)
	}
	result := taskResult{SessionSHA256: sessionSHA256, OutputSHA256: outputSHA256, Evidence: evidence}
	if err := json.NewEncoder(resultOut).Encode(result); err != nil {
		return fmt.Errorf("writing Pi result: %w", err)
	}
	return nil
}

func createTaskArtifactRoot(home, session string) (string, error) {
	parent, parentPath, err := openTaskDirectory(home, true, ".local", "share", "conseil", "pi")
	if err != nil {
		return "", fmt.Errorf("creating Pi result parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if err := parent.Mkdir(session, 0o700); err != nil {
		return "", fmt.Errorf("creating fresh Pi result directory: %w", err)
	}
	info, err := parent.Lstat(session)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("fresh Pi result path is not a directory")
	}
	return filepath.Join(parentPath, session), nil
}

func openTaskDirectory(home string, create bool, components ...string) (*os.Root, string, error) {
	absolute, err := filepath.Abs(home)
	if err != nil {
		return nil, "", err
	}
	homeInfo, err := os.Lstat(absolute)
	if err != nil || !homeInfo.IsDir() || homeInfo.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("home path is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, "", err
	}
	current, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, "", err
	}
	openedHome, err := current.Stat(".")
	if err != nil || !os.SameFile(homeInfo, openedHome) {
		_ = current.Close()
		return nil, "", errors.New("home directory changed while opening")
	}
	currentPath := resolved
	for _, component := range components {
		info, err := current.Lstat(component)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := current.Mkdir(component, 0o700); err != nil {
				_ = current.Close()
				return nil, "", err
			}
			info, err = current.Lstat(component)
		}
		if err != nil {
			_ = current.Close()
			return nil, "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = current.Close()
			return nil, "", fmt.Errorf("task artifact directory %q is not a directory", component)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			_ = current.Close()
			return nil, "", err
		}
		openedInfo, err := next.Stat(".")
		if err != nil || !os.SameFile(info, openedInfo) {
			_ = next.Close()
			_ = current.Close()
			return nil, "", errors.New("task artifact directory changed while opening")
		}
		_ = current.Close()
		current = next
		currentPath = filepath.Join(currentPath, component)
	}
	return current, currentPath, nil
}

func installTaskSession(source string, destinationRoot *os.Root, destination string) error {
	input, err := openRegularFile(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	if info, err := destinationRoot.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("fixed session artifact is a symlink")
		}
		return errors.New("fixed session artifact already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, temporaryName, err := createArtifactTemp(destinationRoot, destination)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = destinationRoot.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := destinationRoot.Rename(temporaryName, destination); err != nil {
		return err
	}
	keep = true
	directory, err := destinationRoot.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func streamTaskArtifact(session, artifact string, output io.Writer) error {
	if err := validateSessionID(session); err != nil {
		return err
	}
	if artifact != taskOutputArtifact && artifact != taskSessionArtifact {
		return fmt.Errorf("invalid task artifact %q", artifact)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home: %w", err)
	}
	file, err := openTaskArtifact(home, session, artifact)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := io.Copy(output, file); err != nil {
		return fmt.Errorf("streaming task artifact: %w", err)
	}
	return nil
}

func hashTaskArtifact(home, session, artifact string) (string, error) {
	file, err := openTaskArtifact(home, session, artifact)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func openTaskArtifact(home, session, artifact string) (*os.File, error) {
	root, _, err := openTaskDirectory(home, false, ".local", "share", "conseil", "pi", session)
	if err != nil {
		return nil, fmt.Errorf("opening task artifact directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(artifact)
	if err != nil {
		return nil, fmt.Errorf("inspecting task artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("task artifact is not a regular file")
	}
	file, err := root.Open(artifact)
	if err != nil {
		return nil, fmt.Errorf("opening task artifact: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("task artifact changed while opening")
	}
	if openedInfo.Size() > maxPiArtifactSize {
		_ = file.Close()
		return nil, fmt.Errorf("task artifact exceeds %d bytes", maxPiArtifactSize)
	}
	return file, nil
}

func openRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("file is not regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("file changed while opening")
	}
	if openedInfo.Size() > maxPiArtifactSize {
		_ = file.Close()
		return nil, fmt.Errorf("file exceeds %d bytes", maxPiArtifactSize)
	}
	return file, nil
}

func decodeTaskRequest(reader io.Reader) (taskRequest, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, maxTaskRequest+1))
	if err != nil {
		return taskRequest{}, fmt.Errorf("reading task request: %w", err)
	}
	if len(contents) > maxTaskRequest {
		return taskRequest{}, errors.New("task request exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var request taskRequest
	if err := decoder.Decode(&request); err != nil {
		return taskRequest{}, fmt.Errorf("decoding task request: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return taskRequest{}, errors.New("task request has trailing data")
	}
	if strings.TrimSpace(request.Task) == "" {
		return taskRequest{}, errors.New("task is empty")
	}
	if err := validateProjectEnvironment(request.Environment); err != nil {
		return taskRequest{}, fmt.Errorf("setup ready environment: %w", err)
	}
	return request, nil
}

func mergeEnvironment(base []string, additions map[string]string) []string {
	result := make([]string, 0, len(base)+len(additions))
	for _, value := range base {
		key, _, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		if _, replaced := additions[key]; !replaced {
			result = append(result, value)
		}
	}
	for key, value := range additions {
		result = append(result, key+"="+value)
	}
	return result
}

func findPiSession(root string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("pi session directory contains symlink %s", path)
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("finding Pi session: %w", err)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("found %d Pi session files, want exactly one", len(matches))
	}
	absolute, err := filepath.Abs(matches[0])
	if err != nil {
		return "", fmt.Errorf("resolving Pi session path: %w", err)
	}
	return absolute, nil
}

func verifyPiOutput(path string) error {
	input, err := openRegularFile(path)
	if err != nil {
		return fmt.Errorf("opening output: %w", err)
	}
	defer func() { _ = input.Close() }()

	verifiedPath := path + ".verified"
	output, err := os.OpenFile(verifiedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating verified output: %w", err)
	}
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(verifiedPath)
		}
	}()

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxPiOutputLine)
	line := 0
	sawAgentEnd := false
	settled := false
	for scanner.Scan() {
		line++
		if settled {
			return fmt.Errorf("event after agent_settled on line %d", line)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &fields); err != nil {
			return fmt.Errorf("decoding output line %d: %w", line, err)
		}
		var eventType string
		rawType, ok := fields["type"]
		if !ok || json.Unmarshal(rawType, &eventType) != nil || eventType == "" {
			return fmt.Errorf("output line %d has no string event type", line)
		}
		canonical, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding output line %d: %w", line, err)
		}
		if _, err := output.Write(append(canonical, '\n')); err != nil {
			return fmt.Errorf("writing verified output: %w", err)
		}
		switch eventType {
		case "agent_end":
			sawAgentEnd = true
		case "agent_settled":
			if !sawAgentEnd {
				return fmt.Errorf("agent_settled before agent_end on line %d", line)
			}
			settled = true
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading output: %w", err)
	}
	if !settled {
		return errors.New("output ended without agent_settled")
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("syncing verified output: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("closing verified output: %w", err)
	}
	if err := input.Close(); err != nil {
		return fmt.Errorf("closing raw output: %w", err)
	}
	if err := os.Rename(verifiedPath, path); err != nil {
		return fmt.Errorf("installing verified output: %w", err)
	}
	keep = true
	return nil
}

func verifyPiSession(path string) (PiEvidence, error) {
	file, err := openRegularFile(path)
	if err != nil {
		return PiEvidence{}, fmt.Errorf("opening session: %w", err)
	}
	defer func() { _ = file.Close() }()
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type entry struct {
		Type          string `json:"type"`
		Provider      string `json:"provider"`
		ModelID       string `json:"modelId"`
		ThinkingLevel string `json:"thinkingLevel"`
		Message       struct {
			Role       string          `json:"role"`
			Provider   string          `json:"provider"`
			Model      string          `json:"model"`
			StopReason string          `json:"stopReason"`
			Content    json.RawMessage `json:"content"`
			Usage      struct {
				Reasoning int64 `json:"reasoning"`
			} `json:"usage"`
		} `json:"message"`
	}

	evidence := PiEvidence{Provider: piProvider, Model: piModel, ThinkingLevel: piThinking}
	thinking := ""
	terminalStopReason := ""
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	line := 0
	for scanner.Scan() {
		line++
		var current entry
		if err := json.Unmarshal(scanner.Bytes(), &current); err != nil {
			return PiEvidence{}, fmt.Errorf("decoding session line %d: %w", line, err)
		}
		switch current.Type {
		case "thinking_level_change":
			thinking = current.ThinkingLevel
		case "message":
			if current.Message.Role != "assistant" {
				continue
			}
			if current.Message.Provider != piProvider || current.Message.Model != piModel {
				return PiEvidence{}, fmt.Errorf("assistant message %d used %s/%s", evidence.AssistantMessages+1, current.Message.Provider, current.Message.Model)
			}
			if thinking != piThinking {
				return PiEvidence{}, fmt.Errorf("assistant message %d used effective thinking level %q", evidence.AssistantMessages+1, thinking)
			}
			evidence.AssistantMessages++
			evidence.ReasoningTokens += current.Message.Usage.Reasoning
			terminalStopReason = current.Message.StopReason
			var blocks []contentBlock
			if err := json.Unmarshal(current.Message.Content, &blocks); err != nil {
				return PiEvidence{}, fmt.Errorf("decoding assistant content on session line %d: %w", line, err)
			}
			var text strings.Builder
			for _, block := range blocks {
				if block.Type == "text" {
					text.WriteString(block.Text)
				}
			}
			if text.Len() > 0 {
				evidence.Output = text.String()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return PiEvidence{}, fmt.Errorf("reading session: %w", err)
	}
	if evidence.AssistantMessages == 0 {
		return PiEvidence{}, errors.New("session has no assistant messages")
	}
	if evidence.ReasoningTokens == 0 {
		return PiEvidence{}, errors.New("session reports no reasoning tokens")
	}
	if terminalStopReason != "stop" {
		return PiEvidence{}, fmt.Errorf("terminal assistant message has unsuccessful stopReason %q", terminalStopReason)
	}
	return evidence, nil
}
