package web

import "embed"

//go:embed templates static
var webFS embed.FS
