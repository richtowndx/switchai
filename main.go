package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"switchai/appdata"
	"switchai/config"
	"switchai/history"
	"switchai/logger"
	"switchai/proxy"
	"switchai/service"
	"switchai/stats"
	"switchai/update"
	"switchai/web"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// 版本信息（编译时通过 -ldflags 注入）
var (
	versionMajor = "0"
	versionMinor = "0"
	versionPatch = "0"
	gitCommit    = ""
)

func init() {
	update.InitWithCommitStr(versionMajor, versionMinor, versionPatch, gitCommit)
}

func main() {
	// Parse command line flags
	port := flag.String("p", "7777", "Port to listen on")
	install := flag.Bool("install", false, "Install as system service")
	uninstall := flag.Bool("uninstall", false, "Uninstall system service")
	skipAuth := flag.Bool("skip", false, "Skip authentication (for internal network deployment)")
	reset2FA := flag.Bool("reset", false, "Reset 2FA data and redirect to first-time binding")
	flag.Parse()

	// Set skip auth mode in config
	if *skipAuth {
		config.SetSkipAuth(true)
	}

	// Initialize data directory first for reset operation
	if err := appdata.Init(); err != nil {
		log.Fatalf("Failed to initialize data directory: %v", err)
	}

	// 初始化日志系统
	if err := logger.Init(); err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}

	// Initialize config first for reset operation
	if err := config.Init(); err != nil {
		logger.Error("Failed to initialize config: %v", err)
		log.Fatalf("Failed to initialize config: %v", err)
	}

	// Handle 2FA reset
	if *reset2FA {
		cfg := config.GetConfig()
		if err := cfg.ResetTOTP(); err != nil {
			logger.Error("Failed to reset 2FA: %v", err)
			fmt.Fprintf(os.Stderr, "Failed to reset 2FA: %v\n", err)
			os.Exit(1)
		}
		logger.Info("2FA has been reset successfully")
		fmt.Println("✅ 2FA 数据已重置，访问页面将跳转到首次绑定")
		// Also clear all sessions to force re-login
		cfg.ClearAllSessionTokens()
		os.Exit(0)
		return
	}

	// Handle service installation/uninstallation
	if *install {
		if err := service.Install(*port); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *uninstall {
		if err := service.Uninstall(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to uninstall service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Normal startup
	startServer(*port)
}

func startServer(port string) {
	// 初始化统计
	stats.Init()

	// 初始化历史记录
	if err := history.Init(); err != nil {
		logger.Error("Failed to initialize history: %v", err)
	}

	// ============================================================
	// 创建连接跟踪器
	// ============================================================
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracker := &proxy.ConnectionTracker{}
	tracker.StartMonitor(ctx) // 启动连接监控协程

	// ============================================================
	// 创建 Gin 路由
	// ============================================================
	r := gin.New()

	// 添加中间件
	r.Use(gin.Recovery())
	r.Use(logger.RequestLogger())
	r.Use(cors.Default())

	// 注册所有路由
	// 1. Web UI 界面路由
	web.RegisterRoutes(r)

	// 2. API 代理路由 (Anthropic/OpenAI/Copilot 统一处理)
	proxy.RegisterRoutes(r)

	// ============================================================
	// 创建监听器（使用 TrackedListener）
	// ============================================================
	addr := ":" + port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	// 包装为 TrackedListener 以跟踪连接
	trackedLn := &proxy.TrackedListener{
		Listener: ln,
		Tracker:  tracker,
		Ctx:      ctx,
	}

	logger.Info("Listener created on %s (with connection tracking)", addr)

	// ============================================================
	// 创建并启动服务器
	// ============================================================
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 设置连接状态回调以更好地跟踪连接
		ConnState: func(conn net.Conn, state http.ConnState) {
			// 可以在这里添加连接状态变化跟踪
			switch state {
			case http.StateNew:
				logger.Info("[CONN] New connection detected")
			case http.StateActive:
				logger.Info("[CONN] Connection became active")
			case http.StateIdle:
				logger.Info("[CONN] Connection idle")
			case http.StateClosed:
				logger.Info("[CONN] Connection closed")
			}
		},
	}

	// 启动服务器
	go func() {
		logger.Info("Starting SwitchAI HTTP service on %s", addr)
		fmt.Printf("\n🚀 SwitchAI is running on http://localhost%s\n\n", port)
		fmt.Printf("📊 Connection tracking: ENABLED\n\n")

		// 使用 TrackedListener 启动 HTTP
		if err := srv.Serve(trackedLn); err != nil && err != http.ErrServerClosed {
			logger.Error("Failed to start server: %v", err)
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")
	fmt.Println("\n🛑 正在关闭服务器...")

	// 打印最终连接统计
	tracker.PrintStats()

	// 关闭数据库连接
	config.Shutdown()

	// 立即保存统计数据
	stats.Shutdown()

	// 关闭历史记录后台保存
	history.Shutdown()

	// 取消上下文（停止监控协程）
	cancel()

	// 优雅关闭服务器
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	// 先关闭监听器（停止接受新连接）
	if err := ln.Close(); err != nil {
		logger.Error("Failed to close listener: %v", err)
	}

	// 然后关闭服务器
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server forced to shutdown: %v", err)
	}

	logger.Info("Server exited")
	fmt.Println("✅ 服务器已安全退出")
}
