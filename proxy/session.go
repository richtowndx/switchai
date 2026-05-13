package proxy

import (
	"sync"
	"sync/atomic"
)

// ConnectionTracker 连接跟踪器
type ConnectionTracker struct {
	connections sync.Map // map[string]*ConnectionInfo
	totalConns  atomic.Int64
	activeConns atomic.Int64
}
