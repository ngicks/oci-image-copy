package cli

import (
	"errors"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

const (
	ExitOk      = 0
	ExitUsage   = 1
	ExitFailure = 2
)

// UsageError marks an error caused by the command line itself, such as an unknown flag or a
// missing argument.
type UsageError struct {
	Err error
}

func (e *UsageError) Error() string {
	return e.Err.Error()
}

func (e *UsageError) Unwrap() error {
	return e.Err
}

// Usage marks err as a usage error; nil stays nil.
func Usage(err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err}
}

// ExitCode maps err to the process exit status: 0 for nil, 1 for a usage error, 2 for anything
// else, including failed entries and interruption by a signal.
func ExitCode(err error) int {
	if err == nil {
		return ExitOk
	}
	if _, ok := errors.AsType[*UsageError](err); ok || ociimagecopy.IsUsage(err) {
		return ExitUsage
	}
	return ExitFailure
}
