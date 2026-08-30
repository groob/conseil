package devbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type command struct {
	Name   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type runCommand func(context.Context, command) error

func osCommand(ctx context.Context, command command) error {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Env = command.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Dir = command.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = command.Stdin, command.Stdout, command.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", command.Name, err)
	}
	return nil
}

func runOutput(ctx context.Context, run runCommand, command command) ([]byte, error) {
	var output bytes.Buffer
	command.Stdout = &output
	if err := run(ctx, command); err != nil {
		return output.Bytes(), err
	}
	return output.Bytes(), nil
}

func shellQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("value contains a character unsafe for a remote shell")
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'", nil
}
