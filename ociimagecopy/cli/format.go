package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// plural spells n with word, or with word's plural form: "entry" becomes "entries", any other
// word gets an "s".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	if stem, ok := strings.CutSuffix(word, "y"); ok {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// errWriter keeps the first write error, so a renderer can write freely and check once.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = fmt.Errorf("writing output: %w", err)
	}
	return n, e.err
}
