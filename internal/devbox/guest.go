package devbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterbourgon/ff/v4"
)

const credentialMintTimeout = 10 * time.Second

// DispatchGuest runs devbox's guest-side commands and executable-name helpers.
// It reports whether args selected a guest command.
func DispatchGuest(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	name := strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	mint := func(ctx context.Context) (string, error) {
		mintCtx, cancel := context.WithTimeout(ctx, credentialMintTimeout)
		defer cancel()
		return mintToken(mintCtx, http.DefaultClient)
	}
	switch name {
	case "git":
		spec, err := gitProcess(args[1:], os.Environ(), false)
		if err != nil {
			return true, err
		}
		return true, execProcess(spec)
	case "gh":
		spec, err := ghProcess(ctx, args[1:], os.Environ(), mint)
		if err != nil {
			return true, err
		}
		return true, execProcess(spec)
	case "vitrier-credential":
		if len(args) != 2 {
			return true, errors.New("credential helper requires get, store, or erase")
		}
		return true, credentialHelper(ctx, args[1], os.Stdin, os.Stdout, mint)
	}
	if len(args) < 2 {
		return false, nil
	}
	switch args[1] {
	case "bootstrap":
		fs := ff.NewFlagSet("bootstrap")
		session := fs.String('s', "session", "", "session ID")
		project := fs.String('p', "project", "", "project")
		ref := fs.String('r', "ref", "", "exact commit")
		branch := fs.String('b', "branch", "", "unique branch")
		workspace := fs.String('w', "workspace", "", "workspace")
		if err := ff.Parse(fs, args[2:]); err != nil {
			return true, err
		}
		return true, bootstrap(ctx, bootstrapRequest{
			session:   *session,
			project:   *project,
			ref:       *ref,
			branch:    *branch,
			workspace: *workspace,
		}, os.Stdout, os.Stderr)
	case "run-task":
		fs := ff.NewFlagSet("run-task")
		session := fs.String('s', "session", "", "session ID")
		workspace := fs.String('w', "workspace", "", "workspace")
		if err := ff.Parse(fs, args[2:]); err != nil {
			return true, err
		}
		return true, runTask(ctx, *session, *workspace, os.Stdin, os.Stderr, os.Stdout)
	case "stream-artifact":
		fs := ff.NewFlagSet("stream-artifact")
		session := fs.String('s', "session", "", "session ID")
		artifact := fs.String('a', "artifact", "", "artifact name")
		if err := ff.Parse(fs, args[2:]); err != nil {
			return true, err
		}
		return true, streamTaskArtifact(*session, *artifact, os.Stdout)
	case "inspect-guest":
		fs := ff.NewFlagSet("inspect-guest")
		workspace := fs.String('w', "workspace", "", "workspace")
		project := fs.String('p', "project", "", "project")
		branch := fs.String('b', "branch", "", "branch")
		base := fs.String('r', "base", "", "base commit")
		if err := ff.Parse(fs, args[2:]); err != nil {
			return true, err
		}
		facts, err := inspectGuestRepository(ctx, *workspace, *project, *branch, *base)
		if err != nil {
			return true, err
		}
		return true, json.NewEncoder(os.Stdout).Encode(facts)
	default:
		return false, nil
	}
}
