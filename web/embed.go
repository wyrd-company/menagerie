// Package web embeds the built web UI so the workspace service can serve it.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS is the built UI rooted at its output folder: index.html and assets/.
// Before `pnpm --dir web build` runs it holds only the .gitkeep placeholder.
var FS fs.FS = sub(dist, "dist")

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}
