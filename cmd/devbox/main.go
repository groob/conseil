// Command devbox provisions and manages task-scoped exe.dev workers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/groob/conseil/internal/devbox"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if handled, err := devbox.DispatchGuest(ctx, os.Args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	cmd := command()
	if err := cmd.ParseAndRun(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, ff.ErrHelp) || errors.Is(err, ff.ErrNoExec) {
			selected := cmd.GetSelected()
			if selected == nil {
				selected = cmd
			}
			if _, writeErr := ffhelp.Command(selected).WriteTo(os.Stdout); writeErr != nil {
				fmt.Fprintln(os.Stderr, writeErr)
				os.Exit(1)
			}
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command() *ff.Command {
	createFlags := ff.NewFlagSet("create")
	createDB := createFlags.String('d', "db", devbox.DefaultDBPath(), "SQLite database path")
	project := createFlags.String('p', "project", "", "lowercase repository name")
	task := createFlags.String('t', "task", "", "task description")
	ref := createFlags.String('r', "ref", "", "full base commit")
	workspace := createFlags.String('w', "workspace", "$HOME/workspace", "guest workspace")
	worker := createFlags.String(0, "worker", "", "Linux devbox worker path")
	artifacts := createFlags.String(0, "artifacts", devbox.DefaultArtifactDir(), "host artifact directory")
	create := &ff.Command{Name: "create", ShortHelp: "create and bootstrap a devbox", Flags: createFlags, Exec: func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return errors.New("create takes no positional arguments")
		}
		request, err := devbox.NewCreateRequest(*project, *task, *ref, *workspace)
		if err != nil {
			return err
		}
		workerPath := *worker
		var cleanup func()
		if workerPath == "" {
			workerPath, cleanup, err = buildWorker(ctx)
			if err != nil {
				return err
			}
			defer cleanup()
		}
		return withFactory(ctx, *createDB, func(factory *devbox.Factory) error {
			session, createErr := factory.Create(ctx, request, workerPath, *artifacts, os.Stdout)
			if session.ID != "" {
				fmt.Fprintf(os.Stderr, "devbox session %s (%s)\n", session.ID, session.VMName)
			}
			return createErr
		})
	}}

	inspectFlags := ff.NewFlagSet("inspect")
	inspectDB := inspectFlags.String('d', "db", devbox.DefaultDBPath(), "SQLite database path")
	inspect := &ff.Command{Name: "inspect", ShortHelp: "inspect durable and live state", Flags: inspectFlags, Exec: func(ctx context.Context, args []string) error {
		if len(args) != 1 {
			return errors.New("usage: devbox inspect SESSION")
		}
		return withFactory(ctx, *inspectDB, func(factory *devbox.Factory) error {
			value, err := factory.Inspect(ctx, args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(value)
		})
	}}

	listFlags := ff.NewFlagSet("list")
	listDB := listFlags.String('d', "db", devbox.DefaultDBPath(), "SQLite database path")
	listProject := listFlags.String('p', "project", "", "filter by project")
	list := &ff.Command{Name: "list", ShortHelp: "list durable sessions", Flags: listFlags, Exec: func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return errors.New("list takes no positional arguments")
		}
		return withFactory(ctx, *listDB, func(factory *devbox.Factory) error {
			sessions, err := factory.List(ctx, *listProject)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(sessions)
		})
	}}

	destroyFlags := ff.NewFlagSet("destroy")
	destroyDB := destroyFlags.String('d', "db", devbox.DefaultDBPath(), "SQLite database path")
	force := destroyFlags.Bool('f', "force", "bypass repository safety checks")
	destroy := &ff.Command{Name: "destroy", ShortHelp: "safely destroy a devbox", Flags: destroyFlags, Exec: func(ctx context.Context, args []string) error {
		if len(args) != 1 {
			return errors.New("usage: devbox destroy SESSION")
		}
		return withFactory(ctx, *destroyDB, func(factory *devbox.Factory) error {
			return factory.Destroy(ctx, args[0], *force)
		})
	}}
	return &ff.Command{
		Name:        "devbox",
		Usage:       "devbox <command> [flags]",
		Subcommands: []*ff.Command{create, inspect, list, destroy},
		Exec: func(_ context.Context, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q", args[0])
			}
			return ff.ErrHelp
		},
	}
}

func withFactory(ctx context.Context, path string, run func(*devbox.Factory) error) (resultErr error) {
	factory, err := devbox.OpenFactory(ctx, path, os.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		if err := factory.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("closing devbox factory: %w", err))
		}
	}()
	return run(factory)
}

func buildWorker(ctx context.Context) (string, func(), error) {
	root, err := moduleRoot()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "conseil-devbox-build-")
	if err != nil {
		return "", nil, fmt.Errorf("creating build directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "devbox")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, "./cmd/devbox")
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("cross-building devbox worker: %w", err)
	}
	return path, cleanup, nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find go.mod")
		}
		dir = parent
	}
}
