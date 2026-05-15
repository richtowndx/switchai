package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"switchai/appdata"
	"sync"
	"time"
)

var (
	InfoLogger  *log.Logger
	WarnLogger  *log.Logger
	ErrorLogger *log.Logger
	currentDate string
	logMutex    sync.Mutex
	infoFile    *os.File
	errorFile   *os.File
)

// 日志文件保留天数
const logRetentionDays = 3

// 日志文件名前缀模式：info_2006-01-02_15.log 或 error_2006-01-02_15.log
var logFilenamePattern = regexp.MustCompile(`^(info|error)_\d{4}-\d{2}-\d{2}_\d{2}\.log$`)

// Init 初始化日志系统
func Init() error {
	// 创建 logs 目录
	logsDir := appdata.GetLogDir()
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return err
	}

	// 初始化日志文件
	if err := rotateLogFiles(); err != nil {
		return err
	}

	// 启动日志轮转检查协程
	go checkLogRotation()

	// 启动日志清理协程
	go cleanupOldLogs()

	log.Println("Logger initialized successfully")
	return nil
}

// rotateLogFiles 轮转日志文件
func rotateLogFiles() error {
	logMutex.Lock()
	defer logMutex.Unlock()

	// 获取当前日期时间
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	timeStr := now.Format("2006-01-02_15")

	// 如果日期没变，不需要轮转
	if currentDate == dateStr && infoFile != nil && errorFile != nil {
		return nil
	}

	// 关闭旧文件
	if infoFile != nil {
		infoFile.Close()
	}
	if errorFile != nil {
		errorFile.Close()
	}

	// 创建新的日志文件
	logsDir := appdata.GetLogDir()
	infoFilename := filepath.Join(logsDir, fmt.Sprintf("info_%s.log", timeStr))
	errorFilename := filepath.Join(logsDir, fmt.Sprintf("error_%s.log", timeStr))

	var err error
	infoFile, err = os.OpenFile(infoFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	errorFile, err = os.OpenFile(errorFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	// 创建多输出：同时输出到文件和控制台
	infoWriter := io.MultiWriter(os.Stdout, infoFile)
	warnWriter := io.MultiWriter(os.Stderr, infoFile)
	errorWriter := io.MultiWriter(os.Stderr, errorFile)

	// 初始化日志记录器
	InfoLogger = log.New(infoWriter, "[INFO] ", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)
	WarnLogger = log.New(warnWriter, "[WARN] ", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)
	ErrorLogger = log.New(errorWriter, "[ERROR] ", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)

	// 设置默认日志输出
	log.SetOutput(infoWriter)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	currentDate = dateStr
	return nil
}

// checkLogRotation 检查是否需要轮转日志
func checkLogRotation() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		dateStr := time.Now().Format("2006-01-02")
		if dateStr != currentDate {
			if err := rotateLogFiles(); err != nil {
				log.Printf("Failed to rotate log files: %v", err)
			}
		}
	}
}

// cleanupOldLogs 清理过期的日志文件
func cleanupOldLogs() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		cleanupLogs()
	}
}

// cleanupLogs 清理超过保留天数的日志文件
func cleanupLogs() {
	logMutex.Lock()
	defer logMutex.Unlock()

	logsDir := appdata.GetLogDir()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		log.Printf("Failed to read logs directory: %v", err)
		return
	}

	cutoff := time.Now().AddDate(0, 0, -logRetentionDays)
	deletedCount := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !logFilenamePattern.MatchString(filename) {
			continue
		}

		// 解析文件名中的日期：info_2006-01-02_15.log
		dateStr := filename[5 : 5+10] // 提取 "2006-01-02"
		fileDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}

		// 如果文件日期早于保留期限，删除
		if fileDate.Before(cutoff) {
			filepath := filepath.Join(logsDir, filename)
			if err := os.Remove(filepath); err != nil {
				log.Printf("Failed to delete old log file %s: %v", filename, err)
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 {
		log.Printf("Cleaned up %d old log files", deletedCount)
	}
}

// Info 记录信息日志
func Info(format string, v ...interface{}) {
	if InfoLogger != nil {
		InfoLogger.Printf(format, v...)
	}
}

// Warn 记录警告日志（输出到 stderr + info 日志文件）
func Warn(format string, v ...interface{}) {
	if WarnLogger != nil {
		WarnLogger.Printf(format, v...)
	}
}

// Error 记录错误日志
func Error(format string, v ...interface{}) {
	if ErrorLogger != nil {
		ErrorLogger.Printf(format, v...)
	}
}

// LogRequest 记录请求信息（精简格式，类似history）
func LogRequest(method, path, clientIP, provider string, statusCode int, latency time.Duration) {
	Info("[%s] %s %s | %s | %d | %v",
		method, path, clientIP, provider, statusCode, latency)
}

// LogHistoryRecord 记录历史请求信息
func LogHistoryRecord(id, method, path, clientIP, provider, model string, statusCode int, duration int64, inputTokens, outputTokens int) {
	Info("[%s] %s %s | %s | %s | %d | %dms | in:%d out:%d",
		method, path, clientIP, provider, model, statusCode, duration, inputTokens, outputTokens)
}
