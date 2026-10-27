// Package templateutil centralizes the [text/template] helpers shared by every
// template-rendering call site. One func map means every template sees the same
// functions; one FuncDocs means the help text cannot drift.
package templateutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// FuncMap returns a fresh template function map shared across the project's
// template renderers.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"json": Json,
	}
}

// FuncDoc documents a single helper exposed by FuncMap.
type FuncDoc struct {
	Name  string // bare function name as registered in FuncMap
	Usage string // name plus argument placeholders, e.g. "json VALUE"
	Desc  string // one-line human description
}

// FuncDocs documents every helper in FuncMap, in display order.
func FuncDocs() []FuncDoc {
	return []FuncDoc{
		{Name: "json", Usage: "json VALUE", Desc: "VALUE marshaled as indented JSON"},
	}
}

// FuncHelp renders FuncDocs as an aligned, indented block ending with a newline.
func FuncHelp() string {
	docs := FuncDocs()
	width := 0
	for _, d := range docs {
		width = max(width, len(d.Usage))
	}
	var b strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, d.Usage, d.Desc)
	}
	return b.String()
}

// Json marshals v as indented JSON.
func Json(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
