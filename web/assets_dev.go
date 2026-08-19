//go:build !production

package web

import "embed"

//go:embed dev/*
var assets embed.FS

func Assets() (embed.FS, string) { return assets, "dev" }
