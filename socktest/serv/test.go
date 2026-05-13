package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// 连接信息结构
type ConnectionInfo struct {
	ID         string
	RemoteAddr string
	StartTime  time.Time
	BytesRead  atomic.Int64
	BytesWrite atomic.Int64
	LastActive atomic.Pointer[time.Time]
	UserAgent  string
	// 可以添加更多字段
	Protocol string // HTTP/1.1, HTTP/2
	IsTLS    bool
}

// 连接跟踪器
type ConnectionTracker struct {
	connections sync.Map // map[string]*ConnectionInfo
	totalConns  atomic.Int64
	activeConns atomic.Int64
}

func NewConnectionTracker() *ConnectionTracker {
	return &ConnectionTracker{}
}

// 监控协程：定期清理和打印统计
func (t *ConnectionTracker) StartMonitor(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				t.printStats()
				t.cleanupStaleConnections(5 * time.Minute)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (t *ConnectionTracker) printStats() {
	var activeCount int
	var totalBytesRead, totalBytesWrite int64

	t.connections.Range(func(key, value interface{}) bool {
		info := value.(*ConnectionInfo)
		activeCount++
		totalBytesRead += info.BytesRead.Load()
		totalBytesWrite += info.BytesWrite.Load()
		return true
	})

	fmt.Printf("[STATS] Active: %d, Total: %d, Read: %s, Write: %s\n",
		activeCount,
		t.totalConns.Load(),
		formatBytes(totalBytesRead),
		formatBytes(totalBytesWrite),
	)
}

func (t *ConnectionTracker) cleanupStaleConnections(maxIdle time.Duration) {
	now := time.Now()
	t.connections.Range(func(key, value interface{}) bool {
		info := value.(*ConnectionInfo)
		if lastActive := info.LastActive.Load(); lastActive != nil {
			if now.Sub(*lastActive) > maxIdle {
				fmt.Printf("[CLEANUP] Removing stale connection: %s (idle: %v)\n",
					info.ID, now.Sub(*lastActive))
				t.connections.Delete(key)
				t.activeConns.Add(-1)
			}
		}
		return true
	})
}

// 连接建立时的回调
func (t *ConnectionTracker) OnAccept(conn net.Conn, protocol string, isTLS bool) *ConnectionInfo {
	connID := fmt.Sprintf("conn-%d-%d", time.Now().Unix(), t.totalConns.Add(1))

	info := &ConnectionInfo{
		ID:         connID,
		RemoteAddr: conn.RemoteAddr().String(),
		StartTime:  time.Now(),
		Protocol:   protocol,
		IsTLS:      isTLS,
	}
	now := time.Now()
	info.LastActive.Store(&now)

	t.connections.Store(connID, info)
	t.activeConns.Add(1)

	fmt.Printf("[ACCEPT] %s from %s (proto: %s, tls: %v)\n",
		connID, info.RemoteAddr, protocol, isTLS)

	return info
}

// 连接关闭时的回调
func (t *ConnectionTracker) OnClose(info *ConnectionInfo, err error) {
	duration := time.Since(info.StartTime)
	lastActive := info.LastActive.Load()
	var idleTime time.Duration
	if lastActive != nil {
		idleTime = time.Since(*lastActive)
	}

	status := "normal"
	if err != nil {
		status = "error: " + err.Error()
	}

	fmt.Printf("[CLOSE] %s from %s (duration: %v, idle: %v, read: %s, write: %s, status: %s)\n",
		info.ID, info.RemoteAddr, duration.Truncate(time.Millisecond),
		idleTime.Truncate(time.Millisecond),
		formatBytes(info.BytesRead.Load()),
		formatBytes(info.BytesWrite.Load()),
		status,
	)

	t.connections.Delete(info.ID)
	t.activeConns.Add(-1)
}

// 包装的 net.Conn，用于跟踪读写
type TrackedConn struct {
	net.Conn
	tracker *ConnectionTracker
	info    *ConnectionInfo
	
}

func (c *TrackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.info.BytesRead.Add(int64(n))
		now := time.Now()
		c.info.LastActive.Store(&now)
	}
	return n, err
}

func (c *TrackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.info.BytesWrite.Add(int64(n))
		now := time.Now()
		c.info.LastActive.Store(&now)
	}
	return n, err
}

func (c *TrackedConn) Close() error {
	err := c.Conn.Close()
	c.tracker.OnClose(c.info, err)
	return err
}

// 自定义 Listener
type TrackedListener struct {
	net.Listener
	tracker *ConnectionTracker
	ctx     context.Context
}

func (l *TrackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	// 检测协议（简化版，实际需要更复杂的检测）
	protocol := "HTTP/1.1"
	if _, ok := conn.(*net.TCPConn); ok {
		// 这里可以添加 HTTP/2 检测逻辑
	}

	// 调用连接建立回调
	info := l.tracker.OnAccept(conn, protocol, false) // 假设非 TLS

	// 返回包装的连接
	return &TrackedConn{
		Conn:    conn,
		tracker: l.tracker,
		info:    info,
	}, nil
}

// 主服务结构
type Server struct {
	server  *http.Server
	tracker *ConnectionTracker
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewServer(addr string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	tracker := NewConnectionTracker()

	s := &Server{
		tracker: tracker,
		ctx:     ctx,
		cancel:  cancel,
	}

	// 启动监控
	tracker.StartMonitor(ctx)

	// 创建 HTTP Server
	s.server = &http.Server{
		Addr: addr,
		BaseContext: func(ln net.Listener) context.Context {
			// 创建根上下文，包含 tracker
			baseCtx := context.WithValue(ctx, "tracker", tracker)
			return baseCtx
		},
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			// 为每个连接创建子上下文
			if tc, ok := c.(*TrackedConn); ok {
				ctx = context.WithValue(ctx, "conn_info", tc.info)
				ctx = context.WithValue(ctx, "conn", tc)
			}
			return ctx
		},
		ConnState: func(conn net.Conn, state http.ConnState) {
			// 连接状态变化回调
			if tc, ok := conn.(*TrackedConn); ok {
				switch state {
				case http.StateNew:
					now := time.Now()
					fmt.Println("new time", now)
				case http.StateActive:
					// 请求处理中
					now := time.Now()
					tc.info.LastActive.Store(&now)
					fmt.Printf("[ACTIVE] Connection %s active\n", tc.info.ID)
				case http.StateIdle:
					// 连接空闲
				case http.StateHijacked:
					fmt.Printf("[HIJACK] Connection %s hijacked\n", tc.info.ID)
				case http.StateClosed:
					// 连接关闭（这里其实在 TrackedConn.Close() 中已经处理了）
				}
			}
		},
		// 其他配置
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    30 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	// 设置 Handler
	s.setupRoutes()

	return s
}

func (s *Server) setupRoutes() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 从上下文中获取连接信息
		if info, ok := r.Context().Value("conn_info").(*ConnectionInfo); ok {
			fmt.Fprintf(w, "Hello! Connection: %s, Active: %v\n",
				info.ID, time.Since(info.StartTime))
		} else {
			w.Write([]byte("Hello!"))
		}
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		tracker, _ := r.Context().Value("tracker").(*ConnectionTracker)
		if tracker == nil {
			http.Error(w, "Tracker not found", http.StatusInternalServerError)
			return
		}

		fmt.Fprintf(w, "Connection Statistics:\n")
		fmt.Fprintf(w, "Total connections: %d\n", tracker.totalConns.Load())
		fmt.Fprintf(w, "Active connections: %d\n\n", tracker.activeConns.Load())

		tracker.connections.Range(func(key, value interface{}) bool {
			info := value.(*ConnectionInfo)
			lastActive := info.LastActive.Load()
			var idleTime string
			if lastActive != nil {
				idleTime = time.Since(*lastActive).Truncate(time.Second).String()
			}

			fmt.Fprintf(w, "- %s: %s (alive: %v, idle: %s, read: %s, write: %s)\n",
				info.ID, info.RemoteAddr,
				time.Since(info.StartTime).Truncate(time.Second),
				idleTime,
				formatBytes(info.BytesRead.Load()),
				formatBytes(info.BytesWrite.Load()),
			)
			return true
		})
	})

	mux.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		// 示例：手动关闭服务器
		fmt.Fprintf(w, "Shutting down server...\n")
		go func() {
			time.Sleep(100 * time.Millisecond)
			s.Stop()
		}()
	})

	s.server.Handler = mux
}

func (s *Server) Start() error {
	fmt.Printf("Starting server on %s\n", s.server.Addr)

	// 创建原始 listener
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}

	// 包装 listener
	trackedLn := &TrackedListener{
		Listener: ln,
		tracker:  s.tracker,
		ctx:      s.ctx,
	}

	return s.server.Serve(trackedLn)
}

func (s *Server) Stop() {
	fmt.Println("Shutting down server...")

	// 停止监控
	s.cancel()

	// 优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s.server.Shutdown(ctx)

	// 打印最终统计
	if s.tracker != nil {
		s.tracker.printStats()
	}
}

// 工具函数：格式化字节大小
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// 中间件：记录请求信息
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// 记录到连接信息
		if info, ok := r.Context().Value("conn_info").(*ConnectionInfo); ok {
			info.LastActive.Store(&start)
		}

		// 调用下一个处理器
		next.ServeHTTP(w, r)

		// 记录请求完成
		duration := time.Since(start)
		fmt.Printf("[REQUEST] %s %s %v\n", r.Method, r.URL.Path, duration)
	})
}

func main() {
	// 创建服务器
	server := NewServer(":8080")

	// 添加中间件
	server.server.Handler = loggingMiddleware(server.server.Handler)

	// 启动服务器
	if err := server.Start(); err != nil && err != http.ErrServerClosed {
		fmt.Printf("Server error: %v\n", err)
	}

	fmt.Println("Server stopped")
}
