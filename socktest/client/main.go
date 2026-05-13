package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-h" {
		fmt.Println("Usage: go run main.go [server_addr]")
		fmt.Println("  server_addr: default http://localhost:8080")
		return
	}

	serverAddr := "http://localhost:8080"
	if len(os.Args) > 1 {
		serverAddr = os.Args[1]
	}

	// 使用自定义 Transport 启用 Keep-Alive（HTTP/1.1 默认行为）
	transport := &http.Transport{
		MaxIdleConns:        1, // 只保持1个空闲连接
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     90 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	deadline := time.After(1 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	reqCount := 0

	fmt.Println("=== HTTP 长连接测试客户端 ===")
	fmt.Printf("目标: %s\n", serverAddr)
	fmt.Println("每 5 秒发送一次请求，1 分钟后退出")
	fmt.Println()

	for {
		select {
		case <-deadline:
			fmt.Println("\n⏰ 1 分钟已到，程序退出")
			return
		case <-ticker.C:
			reqCount++
			resp, err := client.Get(serverAddr + "/")
			if err != nil {
				fmt.Printf("[%2d] ❌ 请求失败: %v\n", reqCount, err)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Printf("[%2d] ✅ %s - %s\n", reqCount, time.Now().Format("15:04:05"),
				string(body[:len(body)-1])) // 去掉换行
		}
	}
}
