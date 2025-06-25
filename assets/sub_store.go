package assets

import (
	// embed 用于嵌入静态文件
	_ "embed"
)

// EmbeddedSubStore 嵌入的Sub-Store JavaScript文件
//
//go:embed sub-store.bundle.js.zst
var EmbeddedSubStore []byte

// EmbeddedOverrideYaml 嵌入的覆写配置YAML文件
//
//go:embed ACL4SSR_Online_Full.yaml.zst
var EmbeddedOverrideYaml []byte
