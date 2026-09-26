package main

import "testing"

// TestWantsHealthcheck 验证自检模式开关的识别。
//
// 规格依据：容器 healthcheck 依赖 "--healthcheck" 触发，
// 且必须能容忍 pflag 风格的 "--healthcheck=true"。
func TestWantsHealthcheck(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"无参数_不触发", nil, false},
		{"空参数_不触发", []string{}, false},
		{"裸flag_触发", []string{"--healthcheck"}, true},
		{"显式true_触发", []string{"--healthcheck=true"}, true},
		{"显式1_触发", []string{"--healthcheck=1"}, true},
		{"空值_触发", []string{"--healthcheck="}, true},
		{"显式false_不触发", []string{"--healthcheck=false"}, false},
		{"与其他参数共存_触发", []string{"--http-addr=:9999", "--healthcheck"}, true},
		{"前缀相似但不相等_不触发", []string{"--healthchecker"}, false},
		{"仅前缀_不触发", []string{"--health"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsHealthcheck(tc.args); got != tc.want {
				t.Fatalf("wantsHealthcheck(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestLocalURL 验证监听地址到可直连 URL 的规范化。
//
// ":8080" 这类"监听所有网卡"的地址不能直接用于发起请求（空 host），
// 必须替换为 127.0.0.1，否则自检必然失败。
func TestLocalURL(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"监听所有网卡_补回环", ":8080", "127.0.0.1:8080"},
		{"显式回环_原样", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"自定义端口_保留端口", ":9999", "127.0.0.1:9999"},
		{"显式host_原样", "0.0.0.0:8080", "0.0.0.0:8080"},
		{"缺端口_补默认端口", "127.0.0.1", "127.0.0.1:8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localURL(tc.addr); got != tc.want {
				t.Fatalf("localURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}
