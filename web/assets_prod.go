//go:build production

package web

import "embed"

//go:embed dist/*
var assets embed.FS

func Assets() (embed.FS, string) { return assets, "dist" }
