package proxy

import "encoding/json"

// BaseProvider 定义上游提供商的参数过滤规则
// 基于 hostname 匹配，每个提供商可以有不同 API 格式的过滤配置
type BaseProvider struct {
	Name   string
	Filter map[string][]string // key: API 格式 ("anthropic"/"openai"/"copilot"), value: 需要过滤的参数名列表
}

// builtInProviders 内置提供商的过滤规则（基于 hostname 匹配）
var builtInProviders = map[string]BaseProvider{
	"open.bigmodel.cn": {
		Name: "anthropic",
		Filter: map[string][]string{
			"anthropic": {},
			"openai":    {},
			"copilot":   {},
		},
	},
	"api.minimaxi.com": {
		Name: "minimaxi",
		Filter: map[string][]string{
			"anthropic": {"output_config"},
			"openai":    {"output_config"},
			"copilot":   {"output_config"},
		},
	},
	"api.stepfun.com": {
		Name: "stepfun",
		Filter: map[string][]string{
			"anthropic": {"output_config"},
			"openai":    {"output_config"},
			"copilot":   {"output_config"},
		},
	},
	"qianfan.baidubce.com": {
		Name: "openai",
		Filter: map[string][]string{
			"anthropic": {},
			"openai":    {},
			"copilot":   {"output_config"},
		},
	},
	"aigw-gzgy2.cucloud.cn": {
		Name: "cucloud",
		Filter: map[string][]string{
			"anthropic": {},
			"openai":    {"output_config"},
			"copilot":   {"output_config"},
		},
	},
}

// getFilterParams 根据 hostname 和 API 格式获取需要过滤的参数列表
// 优先查找内置规则，找不到则返回通用默认过滤规则
func getFilterParams(hostname, apiFormat string) []string {
	if bp, ok := builtInProviders[hostname]; ok {
		if params, ok := bp.Filter[apiFormat]; ok {
			return params
		}
	}
	// 默认过滤规则（保守策略：过滤常见的兼容性参数）
	return defaultFilterParams[apiFormat]
}

// defaultFilterParams 通用默认过滤规则（当 hostname 未命中内置规则时使用）
var defaultFilterParams = map[string][]string{
	"anthropic": {},
	"openai":    {"output_config"},
	"copilot":   {"output_config"},
}

// FilterUnsupportedParams 过滤请求中不被上游支持的参数（兼容旧调用，不依赖 hostname）
func FilterUnsupportedParams(reqBody []byte, apiFormat string) []byte {
	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody
	}

	modified := false
	for _, key := range defaultFilterParams[apiFormat] {
		if _, exists := req[key]; exists {
			delete(req, key)
			modified = true
		}
	}

	if !modified {
		return reqBody
	}

	result, err := json.Marshal(req)
	if err != nil {
		return reqBody
	}
	return result
}
