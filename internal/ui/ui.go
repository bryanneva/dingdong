// Package ui exposes the embedded static assets served by the dingdong server.
package ui

import "embed"

//go:embed static
var Static embed.FS
