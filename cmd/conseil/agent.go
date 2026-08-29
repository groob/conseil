package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type llmAgent struct {
	baseURL         string
	model           string
	reasoningEffort string
	instructions    string
	client          *http.Client
}

type emitEvent func(eventType string, payload any) error

type responsesRequest struct {
	Model        string           `json:"model"`
	Reasoning    reasoningRequest `json:"reasoning"`
	Instructions string           `json:"instructions"`
	Input        []chatMessage    `json:"input"`
	Stream       bool             `json:"stream"`
	Store        bool             `json:"store"`
}

type reasoningRequest struct {
	Effort string `json:"effort"`
}

func (a *llmAgent) stream(ctx context.Context, messages []chatMessage, emit emitEvent) (string, error) {
	requestBody := responsesRequest{
		Model:        a.model,
		Reasoning:    reasoningRequest{Effort: a.reasoningEffort},
		Instructions: a.instructions,
		Input:        messages,
		Stream:       true,
		Store:        false,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("encoding model request: %w", err)
	}
	endpoint := strings.TrimRight(a.baseURL, "/") + "/responses"
	if err := emit("model.request", map[string]string{
		"url":  endpoint,
		"body": string(encoded),
	}); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("creating model request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	response, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling model: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if err := emit("model.response.started", map[string]any{
		"status":       response.StatusCode,
		"content_type": response.Header.Get("Content-Type"),
		"request_id":   firstHeader(response.Header, "X-Request-ID", "OpenAI-Request-ID"),
	}); err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if readErr != nil {
			return "", fmt.Errorf("reading model error response: %w", readErr)
		}
		return "", fmt.Errorf("model returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var output strings.Builder
	var finalOutput string
	completed := false
	err = scanServerSentEvents(response.Body, func(eventName string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		if err := emit("model.event", map[string]string{
			"event": eventName,
			"data":  string(data),
		}); err != nil {
			return err
		}

		var message struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Error    *responseError
			Response *responseResult `json:"response"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			return fmt.Errorf("decoding model stream event %q: %w", eventName, err)
		}
		eventType := message.Type
		if eventType == "" {
			eventType = eventName
		}
		switch eventType {
		case "response.output_text.delta", "response.refusal.delta":
			output.WriteString(message.Delta)
			if err := emit("assistant.delta", map[string]string{"delta": message.Delta}); err != nil {
				return err
			}
		case "response.completed":
			completed = true
			if message.Response != nil {
				finalOutput = message.Response.outputText()
			}
		case "response.failed", "error":
			if message.Error != nil && message.Error.Message != "" {
				return errors.New(message.Error.Message)
			}
			if message.Response != nil && message.Response.Error != nil && message.Response.Error.Message != "" {
				return errors.New(message.Response.Error.Message)
			}
			return fmt.Errorf("model stream ended with %s", eventType)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("reading model stream: %w", err)
	}
	if !completed {
		return "", errors.New("model stream ended before response.completed")
	}
	if finalOutput != "" {
		return finalOutput, nil
	}
	if output.Len() == 0 {
		return "", errors.New("model completed without text")
	}
	return output.String(), nil
}

type responseError struct {
	Message string `json:"message"`
}

type responseResult struct {
	Error  *responseError   `json:"error"`
	Output []responseOutput `json:"output"`
}

type responseOutput struct {
	Type    string            `json:"type"`
	Content []responseContent `json:"content"`
}

type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (r responseResult) outputText() string {
	var output strings.Builder
	for _, item := range r.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" || content.Type == "refusal" {
				output.WriteString(content.Text)
			}
		}
	}
	return output.String()
}

func scanServerSentEvents(reader io.Reader, handle func(eventName string, data []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var eventName string
	var dataLines []string

	dispatch := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		data := []byte(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		name := eventName
		eventName = ""
		return handle(name, data)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		} else {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning server-sent events: %w", err)
	}
	return dispatch()
}

func firstHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}
