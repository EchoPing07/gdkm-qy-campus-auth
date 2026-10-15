package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

//go:embed webui/index.html
var indexHTML []byte

// versionHTML 嵌入页副本，启动时把标题旁的 __VERSION__ 占位符替换为真实版本号，
// 替换结果缓存在此（幂等：重复替换无副作用，预留 UpdateConfig 换页场景）。
var versionHTML []byte

// applyVersion 启动时调用一次：把页面里的 __VERSION__ 替换为注入的版本号。
func applyVersion() {
	versionHTML = bytes.ReplaceAll(indexHTML, []byte("__VERSION__"), []byte(Version))
}

// WebUI 服务端：
//   - 页面（SPA）：GET /、/login、/status、/logs、/settings 均返回同一份嵌入页，
//     由前端按 URL 路径切换「登录 / 状态 / 日志 / 设置」视图，深链接可直接刷新；
//   - 鉴权：config.web.password 明文比对；登录成功发随机 cookie（内存态，重启失效）；
//     无 HTTPS / PBKDF2 / CSRF / 防暴破 / 会话过期，仅限可信网络使用。
//   - API（同一 cookie 校验，一个中间件）：GET /api/status、GET/PUT /api/config、
//     POST /api/{probe|login|logout|mode}/{网口}，另有管理会话的 POST /api/login、/api/logout。
type WebServer struct {
	mgr      *Manager
	mu       sync.Mutex
	sessions map[string]struct{}
}

func NewWebServer(mgr *Manager) *WebServer {
	applyVersion()
	return &WebServer{mgr: mgr, sessions: map[string]struct{}{}}
}

func (s *WebServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// 页面路由：同一份 index.html，前端按 pathname 渲染对应视图
	for _, p := range []string{"/{$}", "/login", "/status", "/logs", "/settings"} {
		mux.HandleFunc("GET "+p, s.handleIndex)
	}
	// 管理会话
	mux.HandleFunc("POST /api/login", s.handleSessionLogin)
	mux.HandleFunc("POST /api/logout", s.handleSessionLogout)
	// 业务 API（cookie 校验）
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /api/config", s.auth(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.auth(s.handlePutConfig))
	mux.HandleFunc("POST /api/probe/{key}", s.auth(s.actionHandler(func(w *Worker) ActionResult { return w.OpProbe() })))
	mux.HandleFunc("POST /api/login/{key}", s.auth(s.actionHandler(func(w *Worker) ActionResult { return w.OpLogin() })))
	mux.HandleFunc("POST /api/logout/{key}", s.auth(s.actionHandler(func(w *Worker) ActionResult { return w.OpLogout() })))
	mux.HandleFunc("POST /api/mode/{key}", s.auth(s.handleMode))
	return mux
}

func (s *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(versionHTML)
}

// auth 业务 API 中间件：校验 cookie 中的随机会话 token。
func (s *WebServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("auth"); err == nil {
			s.mu.Lock()
			_, ok := s.sessions[c.Value]
			s.mu.Unlock()
			if ok {
				next(w, r)
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录或会话已失效"})
	}
}

func (s *WebServer) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.mgr.Password())) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "管理密码错误"})
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "会话令牌生成失败"})
		return
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.sessions[token] = struct{}{}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "auth", Value: token, Path: "/", HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *WebServer) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("auth"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "auth", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"now":        time.Now().Unix(),
		"interfaces": s.mgr.Status(),
	})
}

func (s *WebServer) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	data, err := s.mgr.ConfigJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}

func (s *WebServer) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求失败：" + err.Error()})
		return
	}
	notes, err := s.mgr.UpdateConfig(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "设置已保存并即时生效",
		"notes":   notes,
	})
}

// actionHandler 网口动作统一入口（互斥与结果封装在 Worker 内实现）。
func (s *WebServer) actionHandler(op func(*Worker) ActionResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		wk := s.mgr.Worker(key)
		if wk == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "网口不存在：" + key})
			return
		}
		writeJSON(w, http.StatusOK, op(wk))
	}
}

func (s *WebServer) handleMode(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	newMode, err := s.mgr.ToggleMode(key)
	if err != nil {
		writeJSON(w, http.StatusOK, ActionResult{false, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "运行模式已切换为" + modeName(newMode),
		"mode":    newMode,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
