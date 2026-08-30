package devbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const projectSetupProtocol = 1
const maxProtocolLine = 64 << 10
const maxProtocolEvents = 10_000

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedProjectEnvironmentNames = map[string]bool{
	"BASH_ENV":            true,
	"BASHOPTS":            true,
	"CDPATH":              true,
	"CLASSPATH":           true,
	"CURL_CA_BUNDLE":      true,
	"DOCKER_CONFIG":       true,
	"ENV":                 true,
	"GNUPGHOME":           true,
	"HOME":                true,
	"HOSTALIASES":         true,
	"IFS":                 true,
	"JAVA_TOOL_OPTIONS":   true,
	"JDK_JAVA_OPTIONS":    true,
	"LOCALDOMAIN":         true,
	"NETRC":               true,
	"NODE_EXTRA_CA_CERTS": true,
	"NODE_OPTIONS":        true,
	"NODE_PATH":           true,
	"PATH":                true,
	"PERL5LIB":            true,
	"PERL5OPT":            true,
	"PROMPT_COMMAND":      true,
	"PYTHONHOME":          true,
	"PYTHONPATH":          true,
	"REQUESTS_CA_BUNDLE":  true,
	"RES_OPTIONS":         true,
	"RUBYLIB":             true,
	"RUBYOPT":             true,
	"SHELL":               true,
	"SHELLOPTS":           true,
	"SSL_CERT_DIR":        true,
	"SSL_CERT_FILE":       true,
	"XDG_CONFIG_HOME":     true,
	"XDG_DATA_HOME":       true,
	"XDG_RUNTIME_DIR":     true,
	"XDG_STATE_HOME":      true,
	"_JAVA_OPTIONS":       true,
}

var reservedProjectEnvironmentPrefixes = []string{
	"DYLD_",
	"GH_",
	"GIT_",
	"GITHUB_",
	"LD_",
	"OPENAI_",
	"PI_",
	"SSH_",
}

type projectEvent struct {
	Protocol    int               `json:"protocol"`
	Event       string            `json:"event"`
	Operation   string            `json:"operation,omitempty"`
	Step        string            `json:"step,omitempty"`
	State       string            `json:"state,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Message     string            `json:"message,omitempty"`
}

type acceptedEvent func(projectEvent, json.RawMessage) error

func parseProjectSetupProtocol(reader io.Reader, accept acceptedEvent) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxProtocolLine+1)
	terminalEvent := ""
	steps := make(map[string]string)
	lineNumber := 0
	var protocolErr error
	for scanner.Scan() {
		lineNumber++
		if lineNumber > maxProtocolEvents {
			return fmt.Errorf("project protocol exceeds %d events", maxProtocolEvents)
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return fmt.Errorf("project protocol line %d is empty", lineNumber)
		}
		if terminalEvent != "" {
			return fmt.Errorf("project protocol event after %s on line %d", terminalEvent, lineNumber)
		}
		if err := rejectDuplicateJSONFields(line); err != nil {
			return fmt.Errorf("decoding project protocol line %d: %w", lineNumber, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var event projectEvent
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decoding project protocol line %d: %w", lineNumber, err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return fmt.Errorf("project protocol line %d has trailing data", lineNumber)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			return fmt.Errorf("decoding project protocol fields on line %d: %w", lineNumber, err)
		}
		if err := validateProjectEvent(event, fields); err != nil {
			return fmt.Errorf("project protocol line %d: %w", lineNumber, err)
		}
		switch event.Event {
		case "step":
			key := event.Operation + "\x00" + event.Step
			previous := steps[key]
			if event.State == "start" {
				if previous != "" {
					return fmt.Errorf("project protocol step %s/%s started after %s", event.Operation, event.Step, previous)
				}
				steps[key] = "start"
			} else {
				if previous != "start" {
					return fmt.Errorf("project protocol step %s/%s completed without start", event.Operation, event.Step)
				}
				steps[key] = "done"
			}
		case "ready":
			for key, state := range steps {
				if state != "done" {
					return fmt.Errorf("project protocol became ready while step %q is %s", strings.ReplaceAll(key, "\x00", "/"), state)
				}
			}
			terminalEvent = "ready"
		case "error":
			terminalEvent = "error"
			protocolErr = fmt.Errorf("project setup: %s", event.Message)
		}
		canonical, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encoding project protocol line %d: %w", lineNumber, err)
		}
		if err := accept(event, canonical); err != nil {
			return fmt.Errorf("accepting project protocol line %d: %w", lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) || strings.Contains(err.Error(), "token too long") {
			return fmt.Errorf("project protocol line exceeds %d bytes", maxProtocolLine)
		}
		return fmt.Errorf("reading project protocol: %w", err)
	}
	if protocolErr != nil {
		return protocolErr
	}
	if terminalEvent != "ready" {
		return errors.New("project protocol ended without ready event")
	}
	return nil
}

func runProjectSetupProtocol(ctx context.Context, run runCommand, command command, accept acceptedEvent) error {
	reader, writer := io.Pipe()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	parsed := make(chan error, 1)
	go func() {
		err := parseProjectSetupProtocol(reader, accept)
		_ = reader.CloseWithError(err)
		if err != nil {
			cancel()
		}
		parsed <- err
	}()
	command.Stdout = writer
	runErr := run(runCtx, command)
	closeErr := writer.Close()
	protocolErr := <-parsed
	if protocolErr != nil {
		return protocolErr
	}
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return fmt.Errorf("closing project protocol stream: %w", closeErr)
	}
	return nil
}

func validateProjectEvent(event projectEvent, fields map[string]json.RawMessage) error {
	if event.Protocol != projectSetupProtocol {
		return fmt.Errorf("unsupported protocol %d", event.Protocol)
	}
	switch event.Event {
	case "step":
		if event.Operation != "setup" {
			return fmt.Errorf("unsupported setup operation %q", event.Operation)
		}
		if strings.TrimSpace(event.Step) == "" {
			return errors.New("step name is empty")
		}
		if event.State != "start" && event.State != "done" {
			return fmt.Errorf("invalid step state %q", event.State)
		}
		if hasAnyField(fields, "environment", "message") {
			return errors.New("step has fields for another event type")
		}
	case "ready":
		if err := validateProjectEnvironment(event.Environment); err != nil {
			return fmt.Errorf("ready environment: %w", err)
		}
		if hasAnyField(fields, "operation", "step", "state", "message") {
			return errors.New("ready has fields for another event type")
		}
	case "error":
		if event.Operation != "setup" {
			return fmt.Errorf("unsupported setup operation %q", event.Operation)
		}
		if strings.TrimSpace(event.Message) == "" {
			return errors.New("error message is empty")
		}
		if hasAnyField(fields, "step", "state", "environment") {
			return errors.New("error has fields for another event type")
		}
	default:
		return fmt.Errorf("unknown event %q", event.Event)
	}
	return nil
}

func validateProjectEnvironment(environment map[string]string) error {
	for name, value := range environment {
		if !environmentNamePattern.MatchString(name) || value == "" || strings.ContainsRune(value, 0) {
			return errors.New("contains an invalid name or value")
		}
		if isReservedProjectEnvironmentName(name) {
			return fmt.Errorf("contains reserved variable %q", name)
		}
	}
	return nil
}

func isReservedProjectEnvironmentName(name string) bool {
	name = strings.ToUpper(name)
	if reservedProjectEnvironmentNames[name] || name == "PROXY" || strings.HasSuffix(name, "_PROXY") {
		return true
	}
	for _, prefix := range reservedProjectEnvironmentPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate field %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	return walk()
}

func hasAnyField(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, ok := fields[name]; ok {
			return true
		}
	}
	return false
}
