package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var errAdminUsage = errors.New("usage: vitrier-broker grant <vm> <repository> | vitrier-broker revoke <vm> | vitrier-broker check <vm> <repository>")

func runAdmin(args []string, store grantStore, output io.Writer) error {
	if len(args) == 0 {
		return errAdminUsage
	}

	command := args[0]
	wantArgs := map[string]int{"grant": 3, "revoke": 2, "check": 3}[command]
	if wantArgs == 0 || len(args) != wantArgs {
		return errAdminUsage
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s requires root", command)
	}

	switch command {
	case "grant":
		created, err := store.grant(args[1], args[2])
		if err != nil {
			return fmt.Errorf("granting repository access: %w", err)
		}
		result := "existing"
		if created {
			result = "created"
		}
		if _, err := fmt.Fprintln(output, result); err != nil {
			return fmt.Errorf("reporting grant result: %w", err)
		}
		return nil
	case "revoke":
		if err := store.revoke(args[1]); err != nil {
			return fmt.Errorf("revoking repository access: %w", err)
		}
		return nil
	case "check":
		repository, err := store.lookup(args[1])
		if err != nil {
			return fmt.Errorf("checking repository access: %w", err)
		}
		if repository != args[2] {
			return fmt.Errorf("checking repository access: VM %q is granted %q, not %q", args[1], repository, args[2])
		}
		return nil
	}
	return errAdminUsage
}
