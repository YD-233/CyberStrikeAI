//go:build windows

package handler

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// RunCommandWS 提供交互式 Shell：基于 WebSocket 的长会话。
// Windows 没有 PTY（creack/pty 仅 Unix），因此用 cmd.exe 的 stdin/stdout/stderr 管道实现，
// 行为上接近交互式 Shell（可执行命令、查看输出、发送输入）；resize 消息会被解析并忽略。
func (h *TerminalHandler) RunCommandWS(c *gin.Context) {
	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Windows 优先使用 powershell，找不到则退回 cmd.exe
	shell := "powershell.exe"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "cmd.exe"
	}
	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}

	// Shell -> WebSocket：stdout / stderr 实时写回前端
	doneChan := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(doneChan) }) }

	pump := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_ = conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
		closeDone()
	}
	go pump(stdout)
	go pump(stderr)

	// WebSocket -> Shell：前端输入写入 stdin
	conn.SetReadLimit(64 * 1024)
	_ = conn.SetReadDeadline(time.Now().Add(terminalTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(terminalTimeout))
		return nil
	})

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			_ = cmd.Process.Kill()
			break
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		if len(data) == 0 {
			continue
		}
		// resize 消息在 Windows 管道模式下无意义，解析后忽略（保持与前端协议兼容）
		if msgType == websocket.TextMessage && data[0] == '{' {
			var resize terminalResize
			if json.Unmarshal(data, &resize) == nil && resize.Type == "resize" {
				continue
			}
		}
		if _, err := stdin.Write(data); err != nil {
			_ = cmd.Process.Kill()
			break
		}
	}

	<-doneChan
}
