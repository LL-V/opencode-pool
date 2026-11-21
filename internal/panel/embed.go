// Package panel serves the embedded single-file management UI.
package panel

import _ "embed"

// HTML is the white-theme management panel (DESIGN.md §9).
//
//go:embed index.html
var HTML []byte
