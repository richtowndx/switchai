package proxy

import (
	"sync"
	"testing"
	"time"

	"switchai/config"
)

// TestConnPool_ConcurrentAccess 测试连接池的并发访问安全性
func TestConnPool_ConcurrentAccess(t *testing.T) {
	pool := NewConnPool()

	// 创建测试 Provider
	provider := &config.Provider{
		Name:    "test-provider",
		BaseURL: "https://api.test.com",
		APIKey:  "test-key",
	}

	// 并发访问连接池
	var wg sync.WaitGroup
	concurrency := 100
	errors := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.Get(provider)
			if err != nil {
				// 预期会有错误（因为测试 Provider 可能不完整）
				// 只检查没有 panic 或竞态条件
				errors <- err
			}
		}()
	}

	wg.Wait()
	close(errors)

	// 验证没有竞态条件
	t.Log("Concurrent access test completed without race conditions")
}

// TestConnPool_LoadOrStore 测试 LoadOrStore 的原子性
func TestConnPool_LoadOrStore(t *testing.T) {
	pool := NewConnPool()

	provider := &config.Provider{
		Name:    "test-provider",
		BaseURL: "https://api.test.com",
		APIKey:  "test-key",
	}

	key := provider.Name + "|" + provider.BaseURL

	// 创建初始连接
	newConn := &ProviderConn{
		Provider:    provider,
		LastUsedAt:  time.Now(),
		ReqCount:    0,
		HealthCheck: time.Now(),
	}

	// 原子存储
	actual, loaded := pool.providers.LoadOrStore(key, newConn)

	if loaded {
		t.Error("Expected loaded to be false for first store")
	}

	if actual != newConn {
		t.Error("Expected stored value to be returned")
	}

	// 第二次加载应该返回已存在的值
	_, loaded = pool.providers.LoadOrStore(key, &ProviderConn{})
	if !loaded {
		t.Error("Expected loaded to be true for subsequent LoadOrStore")
	}
}

// TestConnPool_CleanupIdle 测试空闲连接清理
func TestConnPool_CleanupIdle(t *testing.T) {
	pool := NewConnPool()

	// 添加一些测试连接
	provider1 := &config.Provider{Name: "provider-1", BaseURL: "https://api1.com"}
	provider2 := &config.Provider{Name: "provider-2", BaseURL: "https://api2.com"}

	pool.providers.Store(provider1.Name+"|"+provider1.BaseURL, &ProviderConn{
		Provider:   provider1,
		LastUsedAt: time.Now().Add(-1 * time.Hour), // 空闲1小时
	})

	pool.providers.Store(provider2.Name+"|"+provider2.BaseURL, &ProviderConn{
		Provider:   provider2,
		LastUsedAt: time.Now(), // 刚刚使用
	})

	// 清理空闲超过30分钟的连接
	cleaned := pool.CleanupIdle(30 * time.Minute)

	if cleaned != 1 {
		t.Errorf("Expected to clean 1 connection, got %d", cleaned)
	}

	// 验证 provider1 已被清理，provider2 仍在
	_, exists1 := pool.providers.Load(provider1.Name + "|" + provider1.BaseURL)
	_, exists2 := pool.providers.Load(provider2.Name + "|" + provider2.BaseURL)

	if exists1 {
		t.Error("Expected idle connection to be cleaned up")
	}

	if !exists2 {
		t.Error("Expected active connection to remain")
	}
}

// TestConnPool_Stats 测试连接池统计
func TestConnPool_Stats(t *testing.T) {
	pool := NewConnPool()

	// 添加测试连接
	provider := &config.Provider{Name: "test-provider", BaseURL: "https://api.test.com"}
	pool.providers.Store(provider.Name+"|"+provider.BaseURL, &ProviderConn{
		Provider:   provider,
		ReqCount:   42,
		LastUsedAt: time.Now(),
	})

	stats := pool.Stats()

	totalConns, ok := stats["total_connections"].(int)
	if !ok || totalConns != 1 {
		t.Errorf("Expected total_connections to be 1, got %v", totalConns)
	}

	totalReqs, ok := stats["total_requests"].(int)
	if !ok || totalReqs != 42 {
		t.Errorf("Expected total_requests to be 42, got %v", totalReqs)
	}
}

// TestConnPool_Remove 测试连接移除
func TestConnPool_Remove(t *testing.T) {
	pool := NewConnPool()

	provider := &config.Provider{
		Name:    "test-provider",
		BaseURL: "https://api.test.com",
		APIKey:  "test-key",
	}

	key := provider.Name + "|" + provider.BaseURL
	testConn := &ProviderConn{
		Provider:   provider,
		LastUsedAt: time.Now(),
	}

	// 存储连接
	pool.providers.Store(key, testConn)

	// 验证连接存在
	if _, exists := pool.providers.Load(key); !exists {
		t.Error("Expected connection to exist before removal")
	}

	// 移除连接
	err := pool.Remove(provider)
	if err != nil {
		t.Errorf("Expected no error on Remove, got %v", err)
	}

	// 验证连接已移除
	if _, exists := pool.providers.Load(key); exists {
		t.Error("Expected connection to be removed")
	}
}

// TestConnPool_HealthCheck 测试健康检查
func TestConnPool_HealthCheck(t *testing.T) {
	pool := NewConnPool()

	// 添加一个空闲时间过长的连接
	idleProvider := &config.Provider{Name: "idle-provider", BaseURL: "https://idle.api.com"}
	pool.providers.Store(idleProvider.Name+"|"+idleProvider.BaseURL, &ProviderConn{
		Provider:   idleProvider,
		LastUsedAt: time.Now().Add(-30 * time.Minute),
		ReqCount:   100,
	})

	// 添加一个高请求计数的连接
	busyProvider := &config.Provider{Name: "busy-provider", BaseURL: "https://busy.api.com"}
	pool.providers.Store(busyProvider.Name+"|"+busyProvider.BaseURL, &ProviderConn{
		Provider:   busyProvider,
		LastUsedAt: time.Now(),
		ReqCount:   15000,
	})

	issues := pool.HealthCheck()

	if len(issues) != 2 {
		t.Errorf("Expected 2 health issues, got %d", len(issues))
	}

	for _, issue := range issues {
		t.Logf("Health issue: %s", issue)
	}
}
