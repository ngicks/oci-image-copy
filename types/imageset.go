// Package types defines the values callers hand to the library:
// the set of images to move, image references, and platforms.
package types

// ImageSetManifest names the images a push, pull or mirror moves.
// Each entry is a reference accepted by [ParseReference].
type ImageSetManifest struct {
	Images []string `json:"images"`
}
