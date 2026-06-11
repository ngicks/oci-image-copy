// Package cmdsignals defines the OS signals that trigger graceful
// shutdown of the oci-image-copy CLI.
package cmdsignals

import (
	"os"
	"syscall"
)

// ExitSignals are the signals that should cancel top-level CLI execution.
var ExitSignals = [...]os.Signal{
	os.Interrupt,
	syscall.SIGTERM,
}
