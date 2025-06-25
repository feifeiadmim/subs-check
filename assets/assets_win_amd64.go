//go:build windows && amd64
// +build windows,amd64

package assets

import (
	// embed 用于嵌入二进制文件
	_ "embed"
)

// EmbeddedNode 嵌入的Node.js二进制文件
//
//go:embed node_windows_amd64.zst
var EmbeddedNode []byte
