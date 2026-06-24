package handler

import (
	"net/http"

	"github.com/gorilla/websocket"
)

// terminalResize is sent by the frontend when the xterm.js terminal is resized.
// 平台无关：Unix(PTY) 用它调整窗口大小；Windows(管道) 仅解析并忽略。
type terminalResize struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// wsUpgrader 仅用于系统设置中的终端 WebSocket，会复用已有的登录保护（JWT 中间件在上层路由组）
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 由于已在 Gin 路由层做了认证，这里放宽 Origin，方便在同一域名下通过 HTTPS/WSS 访问
		return true
	},
}
