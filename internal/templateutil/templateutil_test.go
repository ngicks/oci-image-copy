package templateutil

import (
	"slices"
	"testing"
)

func TestFuncDocs_MatchesFuncMap(t *testing.T) {
	var mapped []string
	for name := range FuncMap() {
		mapped = append(mapped, name)
	}
	var documented []string
	for _, d := range FuncDocs() {
		documented = append(documented, d.Name)
	}
	slices.Sort(mapped)
	slices.Sort(documented)
	if !slices.Equal(mapped, documented) {
		t.Fatalf("FuncMap has %v, FuncDocs documents %v", mapped, documented)
	}
}
