// Package dbbak 提供由后端统一承载的现有中文 Web 资源。
package dbbak

import "embed"

// StaticFS 嵌入 web/ 下的真实前端源码，不维护第二份构建副本。
// 目录必须与包同处模块根（仓库根）：go:embed 只能引用当前目录及子目录。
//
//go:embed web/index.html web/app.js web/style.css web/app-extra.css
var StaticFS embed.FS
