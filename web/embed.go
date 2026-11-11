// Package web holds pixyd's UI: plain HTML, CSS and JavaScript, embedded
// into the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html style.css app.js icons.svg fonts/*.woff2
var files embed.FS

// FS is the UI's file tree.
var FS fs.FS = files
