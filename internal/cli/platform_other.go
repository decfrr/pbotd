//go:build !linux

package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/decfrr/pbotd/internal/config"
)

func runDaemon(config.Config, config.Paths) error {
	return fmt.Errorf("pbotd daemon requires Linux or WSL")
}
func autostart(context.Context, config.Paths) error {
	return fmt.Errorf("pbotd execution requires Linux or WSL")
}
func internalCommand(_ string, _ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "pbotd execution requires Linux or WSL")
	return 1
}
func doctor(_ config.Config, _ config.Paths, _ []string, _ io.Writer, stderr io.Writer) int {
	fmt.Fprintln(stderr, "pbotd diagnostics require Linux or WSL")
	return 1
}
