package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestProjectSetupProtocolMatchesSamovarSetupV1Fixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/samovar-setup-v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var got []projectEvent
	if err := parseProjectSetupProtocol(bytes.NewReader(fixture), func(event projectEvent, _ json.RawMessage) error {
		got = append(got, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := decodeProjectEvents(t, fixture)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed fixture events = %#v, want %#v", got, want)
	}
}

func TestProjectSetupProtocolMatchesSamovarErrorV1Fixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/samovar-error-v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	err = parseProjectSetupProtocol(bytes.NewReader(fixture), discardProjectEvent)
	if got, want := errString(err), "project setup: parse setup arguments: --workspace is required"; got != want {
		t.Errorf("parseProjectSetupProtocol() error = %q, want %q", got, want)
	}
}

func TestProjectSetupProtocolAllowsEmptyOrProjectEnvironment(t *testing.T) {
	for name, input := range map[string]string{
		"empty":   `{"protocol":1,"event":"ready","environment":{}}` + "\n",
		"omitted": `{"protocol":1,"event":"ready"}` + "\n",
		"samovar": `{"protocol":1,"event":"ready","environment":{"SAMOVAR_TEST_DATABASE_URL":"postgres://test"}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseProjectSetupProtocol(strings.NewReader(input), discardProjectEvent); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProjectSetupProtocolRejectsReservedEnvironment(t *testing.T) {
	names := []string{
		"HOME", "PATH", "PI_MODEL", "OPENAI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN",
		"GIT_CONFIG_COUNT", "SSH_AUTH_SOCK", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES",
		"HTTP_PROXY", "https_proxy", "NO_PROXY", "BASH_ENV", "ENV", "NODE_OPTIONS",
		"PYTHONPATH", "RUBYOPT", "SHELL",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			input, err := json.Marshal(projectEvent{Protocol: 1, Event: "ready", Environment: map[string]string{name: "value"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := parseProjectSetupProtocol(bytes.NewReader(append(input, '\n')), discardProjectEvent); err == nil {
				t.Error("parseProjectSetupProtocol() error = nil, want reserved-variable error")
			}
		})
	}
}

func TestProjectSetupProtocolStrict(t *testing.T) {
	cases := map[string]string{
		"old_schema":            "{\"protocol\":1,\"type\":\"ready\",\"environment\":{\"A\":\"b\"}}\n",
		"unknown_field":         "{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"},\"extra\":1}\n",
		"empty_forbidden_field": "{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"},\"operation\":\"\"}\n",
		"null_forbidden_field":  "{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"},\"message\":null}\n",
		"wrong_version":         "{\"protocol\":2,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n",
		"duplicate_field":       "{\"protocol\":1,\"event\":\"ready\",\"event\":\"step\",\"environment\":{\"A\":\"b\"}}\n",
		"unknown_event":         "{\"protocol\":1,\"event\":\"wat\"}\n",
		"missing_operation":     "{\"protocol\":1,\"event\":\"step\",\"step\":\"x\",\"state\":\"start\"}\n",
		"unsupported_operation": "{\"protocol\":1,\"event\":\"step\",\"operation\":\"check\",\"step\":\"x\",\"state\":\"start\"}\n",
		"bad_state":             "{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"working\"}\n",
		"done_without_start":    "{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"done\"}\n",
		"duplicate_start":       "{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"start\"}\n{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"start\"}\n",
		"ready_during_step":     "{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"start\"}\n{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n",
		"missing_ready":         "{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"start\"}\n",
		"after_ready":           "{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n{\"protocol\":1,\"event\":\"step\",\"operation\":\"setup\",\"step\":\"x\",\"state\":\"done\"}\n",
		"error_event":           "{\"protocol\":1,\"event\":\"error\",\"operation\":\"setup\",\"message\":\"failed\"}\n",
		"after_error":           "{\"protocol\":1,\"event\":\"error\",\"operation\":\"setup\",\"message\":\"failed\"}\n{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n",
		"duplicate_ready":       "{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := parseProjectSetupProtocol(strings.NewReader(input), discardProjectEvent); err == nil {
				t.Fatal("accepted invalid protocol")
			}
		})
	}
}

func TestRunProjectSetupProtocolRequiresSuccessfulProcess(t *testing.T) {
	processErr := errors.New("process failed")
	run := func(_ context.Context, command command) error {
		_, _ = io.WriteString(command.Stdout, "{\"protocol\":1,\"event\":\"ready\",\"environment\":{\"A\":\"b\"}}\n")
		return processErr
	}
	if err := runProjectSetupProtocol(t.Context(), run, command{}, discardProjectEvent); !errors.Is(err, processErr) {
		t.Errorf("runProjectSetupProtocol() error = %v, want %v", err, processErr)
	}
}

func TestProjectSetupProtocolRejectsLongLine(t *testing.T) {
	input := strings.Repeat("x", maxProtocolLine+1) + "\n"
	if err := parseProjectSetupProtocol(strings.NewReader(input), discardProjectEvent); err == nil {
		t.Fatal("accepted long line")
	}
}

func discardProjectEvent(projectEvent, json.RawMessage) error { return nil }

func decodeProjectEvents(t *testing.T, fixture []byte) []projectEvent {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(fixture))
	var events []projectEvent
	for {
		var event projectEvent
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}
