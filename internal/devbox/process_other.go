//go:build !darwin && !linux

package devbox

import "context"

func runControlCommand(ctx context.Context, command command) error {
	return osCommand(ctx, command)
}
