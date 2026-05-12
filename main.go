package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"switchai/appdata"
	"switchai/cert"
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
	tls := flag.Bool("tls", false, "Enable TLS/HTTPS (default: plain HTTP)")
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
	startServer(*port, *tls)
}

func startServer(port string, tls bool) {
	// 初始化统计
	stats.Init()

	// 初始化历史记录
	if err := history.Init(); err != nil {
		logger.Error("Failed to initialize history: %v", err)
	}

	// 创建 Gin 路由（不使用 Default，手动添加中间件）
	r := gin.New()

	// 添加恢复中间件
	r.Use(gin.Recovery())

	// 添加请求日志中间件
	r.Use(logger.RequestLogger())

	// 配置 CORS
	r.Use(cors.Default())

	// 管理界面路由
	web.RegisterRoutes(r)

	// Claude API 代理路由
	proxy.RegisterRoutes(r)

	// 启动服务
	addr := ":" + port
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// 启动自动更新器（服务模式下）
	isService := update.IsRunningAsService()
	if isService {
		// updater := update.NewAutoUpdater()
		// updater.SetUpdateCallback(func(result *update.CheckResult) {
		// 	logger.Info("自动下载并安装新版本: %s", result.Latest.String())
		// 	if err := update.DownloadAndInstall(result.DownloadURL); err != nil {
		// 		logger.Error("自动更新失败: %v", err)
		// 		return
		// 	}
		// 	// 更新完成后重启服务
		// 	if err := update.RestartService(); err != nil {
		// 		logger.Error("重启服务失败: %v", err)
		// 	}
		// })
		// go updater.Start()
		// logger.Info("自动更新服务已启动 (服务模式)")
	}

	// 启动服务器
	go func() {
		if tls {
			certPath, keyPath, err := cert.EnsureCertificates(appdata.GetDataDir())
			if err != nil {
				logger.Error("Failed to prepare TLS certificates: %v", err)
				log.Fatalf("Failed to prepare TLS certificates: %v", err)
			}
			logger.Info("Starting SwitchAI HTTPS service on %s", addr)
			fmt.Printf("\n🚀 SwitchAI is running on https://localhost:%s\n\n", port)
			if err := srv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
				logger.Error("Failed to start server: %v", err)
				log.Fatalf("Failed to start server: %v", err)
			}
		} else {
			logger.Info("Starting SwitchAI HTTP service on %s", addr)
			fmt.Printf("\n🚀 SwitchAI is running on http://localhost:%s\n\n", port)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("Failed to start server: %v", err)
				log.Fatalf("Failed to start server: %v", err)
			}
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")
	fmt.Println("\n🛑 正在关闭服务器...")

	// 关闭数据库连接
	config.Shutdown()

	// 立即保存统计数据
	stats.Shutdown()

	// 关闭历史记录后台保存
	history.Shutdown()

	// 优雅关闭服务器
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown: %v", err)
		log.Fatal("Server forced to shutdown:", err)
	}

	logger.Info("Server exited")
	fmt.Println("✅ 服务器已安全退出")
}
