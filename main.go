package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// gdkm-qy-campus-auth —— 广东科贸清远校区校园网认证保活工具
// 形态：Go 单文件静态编译，CGO=0，仅标准库。无 CLI：启动即常驻 + WebUI。
func main() {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[启动失败] 无法定位可执行文件：", err)
		os.Exit(1)
	}
	baseDir := filepath.Dir(exePath)

	cfgPath := filepath.Join(baseDir, "config.json")
	flagConfig := ""
	flag.StringVar(&flagConfig, "config", cfgPath, "配置文件路径")
	flag.Parse()
	cfgPath = flagConfig

	cfg, created, err := loadOrCreateConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[启动失败] ", err)
		os.Exit(1)
	}
	if created {
		fmt.Printf("已生成默认配置 %s（管理密码 password），请尽快修改，不重启亦可于网页设置页修改\n", cfgPath)
	}

	logger := NewLogger()
	if err := logger.Reconfigure(cfg.Log.Level, cfg.Log.File, cfg.Log.MaxMB, baseDir); err != nil {
		fmt.Fprintln(os.Stderr, "[警告] 日志文件不可用（仅输出 stdout）：", err)
	}
	logger.Info("系统", "gdkm-qy-campus-auth 启动，配置文件：%s", cfgPath)

	mgr := NewManager(cfg, cfgPath, baseDir, logger)
	mgr.Start()
	defer mgr.Stop()

	ws := NewWebServer(mgr)
	srv := &http.Server{Addr: cfg.Web.Listen, Handler: ws.Handler()}

	sigCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	shutdownDone := make(chan struct{})
	go func() {
		<-sigCtx.Done()
		logger.Info("系统", "收到退出信号，正在关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(shutdownDone)
	}()

	logger.Info("系统", "WebUI 监听 http://%s", cfg.Web.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("系统", "HTTP 服务异常退出：%v", err)
		os.Exit(1)
	}
	<-shutdownDone
	logger.Info("系统", "已退出")
}
