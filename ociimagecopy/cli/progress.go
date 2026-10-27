package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

// progress renders transfer events: on a terminal, one status line redrawn at most every
// redrawEvery; otherwise one line per finished blob.
type progress struct {
	w    io.Writer
	tty  bool
	mu   sync.Mutex
	blob map[digest.Digest]ociimagecopy.Event
	last time.Time
	drew bool
}

const redrawEvery = 100 * time.Millisecond

func newProgress(w io.Writer, tty bool) *progress {
	return &progress{w: w, tty: tty, blob: map[digest.Digest]ociimagecopy.Event{}}
}

func (p *progress) event(e ociimagecopy.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, seen := p.blob[e.Blob]
	p.blob[e.Blob] = e
	if !p.tty {
		if e.Done == e.Total && (!seen || prev.Done != prev.Total) {
			fmt.Fprintf(p.w, "copied %s (%s) for %s %s\n",
				e.Blob, humanBytes(e.Total), e.Ref, e.Platform)
		}
		return
	}
	if now := time.Now(); now.Sub(p.last) >= redrawEvery || e.Done == e.Total {
		p.last = now
		p.draw()
	}
}

func (p *progress) draw() {
	var done, total int64
	finished := 0
	for _, e := range p.blob {
		done += e.Done
		total += e.Total
		if e.Done == e.Total {
			finished++
		}
	}
	fmt.Fprintf(p.w, "\r\x1b[Kcopying %s / %s, %d of %s done",
		humanBytes(done), humanBytes(total), finished, plural(len(p.blob), "blob"))
	p.drew = true
}

// finish ends the status line so that later output starts on a fresh line.
func (p *progress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && p.drew {
		p.draw()
		fmt.Fprintln(p.w)
	}
}
