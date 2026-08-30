package devbox

import (
	"context"
	"io"
)

// Factory owns devbox persistence and performs host-side lifecycle operations.
type Factory struct {
	store *store
	logs  io.Writer
}

// OpenFactory opens the devbox database at path.
func OpenFactory(ctx context.Context, path string, logs io.Writer) (*Factory, error) {
	store, err := openStore(ctx, path)
	if err != nil {
		return nil, err
	}
	return &Factory{store: store, logs: logs}, nil
}

// Close closes the factory's database.
func (f *Factory) Close() error {
	return f.store.close()
}

// List returns sessions, optionally restricted to project.
func (f *Factory) List(ctx context.Context, project string) ([]Session, error) {
	return f.store.list(ctx, project)
}
