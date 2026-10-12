package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/require"
)

// 本文件证明 2026-10-12 的 RealIP 替换真的有效，不是只消掉了 SA1019 告警。
//
// **为什么需要它**：chi 的 RealIP 已被官方弃用并点名安全缺陷
// （GHSA-3fxj-6jh8-hvhx 等）——它无条件把 r.RemoteAddr 改写成
// X-Forwarded-For 最左值 / X-Real-IP / True-Client-IP。
// 「换成ClientIPFromRemoteAddr 就好了」这句话本身是**不可证伪的口头断言**：
// 若中间件顺序写错、或误用ClientIPFromHeader(攻击者可控的头)，
// 代码照样编译通过、lint 照样零告警，只有真实请求才暴露。
// 故此处用**变异测试**的思路：先证明旧行为确实可被伪造，再证新行为不可。

// newIPTestServer 装配与生产一致的最小中间件链。
//
// 顺序照抄 cmd/iip-server/main.go newRouter：RequestID → ClientIPFromRemoteAddr
// → AccessLog。若把 ClientIPFromRemoteAddr 挪到 AccessLog 之后，
// AccessLog 读到的 GetClientIP 就会是空串 —— 下面有专门用例守这一点。
func newIPTestServer(t *testing.T, remoteAddr string) (*httptest.Server, func() []map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	var logs []map[string]any
	flush := func() []map[string]any {
		logs = nil
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("日志不是合法 JSON: %v | line=%s", err, line)
			}
			logs = append(logs, m)
		}
		return logs
	}

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.ClientIPFromRemoteAddr, AccessLog(logger))
	r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// httptest.NewServer 的 RemoteAddr 是回环地址；这里替换成调用方指定的，
	// 以便验证「TCP 对端」这一语义，而不是绑定到 127.0.0.1 这个常量。
	srv := httptest.NewUnstartedServer(r)
	srv.Listener.Addr() // 触发 listener 已创建
	srv.Start()
	t.Cleanup(srv.Close)

	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.RemoteAddr = remoteAddr
		r.ServeHTTP(w, req)
	})

	return srv, flush
}

// TestAccessLogClientIPIgnoresSpoofedHeaders 是本批的核心判据：
// 客户端伪造 X-Forwarded-For / X-Real-IP / True-Client-IP 后，
// 日志里的 client_ip 必须仍是 TCP 对端。
//
// **变异目标**：把 main.go 的 ClientIPFromRemoteAddr 换回 RealIP，
// 本用例必须变红。真IP 场景下RealIP 会取 XFF 最左值 9.9.9.9。
func TestAccessLogClientIPIgnoresSpoofedHeaders(t *testing.T) {
	const peer = "203.0.113.7:54321" // RFC 5737 文档用网段，语义上不会撞真实主机

	srv, flush := newIPTestServer(t, peer)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/probe", nil)
	require.NoError(t, err)
	// 三个都是 RealIP 会采信的头：X-Forwarded-For、True-Client-IP、X-Real-IP
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	req.Header.Set("True-Client-IP", "8.8.8.8")
	req.Header.Set("X-Real-IP", "7.7.7.7")
	req.Header.Set("X-Client-IP", "6.6.6.6")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	entries := flush()
	require.Len(t, entries, 1, "应恰好一条访问日志")

	const spoofed = "9.9.9.9"
	assertFields := map[string]string{
		"client_ip": "203.0.113.7", // 只能是 TCP 对端
		"peer_addr": "203.0.113.7", // 去掉端口后的对端
	}
	for field, want := range assertFields {
		got, ok := entries[0][field].(string)
		require.Truef(t, ok, "日志缺字段 %s，实际键=%v", field, entries[0])
		require.Equalf(t, want, got, "字段 %s 被伪造请求头污染", field)
	}

	// 反向断言：把被丢弃的值显式写出来。防止「client_ip 为空也算过」
	// 这类空断言假通过 —— 空串 != 9.9.9.9，但那只是没实现，不是实现对了。
	for _, spoofedIP := range []string{spoofed, "8.8.8.8", "7.7.7.7", "6.6.6.6"} {
		for _, field := range []string{"client_ip", "peer_addr"} {
			require.NotEqualf(t, spoofedIP, entries[0][field],
				"字段 %s 竟等于伪造值 %s —— 中间件仍在采信客户端可控的头", field, spoofedIP)
		}
	}

	// peer_addr 不得带端口，且不得出现 raw RemoteAddr 形态。
	require.NotContains(t, entries[0]["peer_addr"], ":54321",
		"peer_addr 应只保留 host，不含端口")
}

// TestAccessLogClientIPFoldedIPv4 守 netip 的 Unmap 语义：
// IPv4 客户端在双栈监听上表现为 ::ffff:a.b.c.d，应折叠成纯 v4。
//
// 折叠的意义是防别名绕过：若后续引入基于前缀的可信列表
// （ClientIPFromXFF），不折叠时攻击者可用两种写法绕过同一份名单。
func TestAccessLogClientIPFoldedIPv4(t *testing.T) {
	srv, flush := newIPTestServer(t, "[::ffff:198.51.100.9]:443")

	resp, err := http.Get(srv.URL + "/probe")
	require.NoError(t, err)
	defer resp.Body.Close()

	entries := flush()
	require.Len(t, entries, 1)
	require.Equal(t, "198.51.100.9", entries[0]["client_ip"],
		"v4-mapped IPv6 应折叠为纯 v4")
	require.Equal(t, "198.51.100.9", entries[0]["peer_addr"],
		"peer_addr 同样应折叠并去端口")
}

// TestAccessLogPeerAddrBareIPWithoutPort 守 httptest/直连场景：
// RemoteAddr 可能是裸 IP（无端口），SplitHostPort 会失败。
// 契约是「原样返回，不吞错也不臆造」。
func TestAccessLogPeerAddrBareIPWithoutPort(t *testing.T) {
	srv, flush := newIPTestServer(t, "198.51.100.55") // 无端口

	resp, err := http.Get(srv.URL + "/probe")
	require.NoError(t, err)
	defer resp.Body.Close()

	entries := flush()
	require.Len(t, entries, 1)
	require.Equal(t, "198.51.100.55", entries[0]["peer_addr"],
		"裸 IP 应原样返回，不被 SplitHostPort 的错误吞掉")
}

// TestPeerAddrUnparseableIsPassedThrough 明确失败语义：
// RemoteAddr 完全不是 host:port 也不是 IP 时，原样返回而不是返回空串 ——
// 返回空串会让「地址缺失」和「解析失败」在日志里无法区分。
func TestPeerAddrUnparseableIsPassedThrough(t *testing.T) {
	r := &http.Request{RemoteAddr: "not-an-address"}
	require.Equal(t, "not-an-address", peerAddr(r))
}

// TestClientIPMissingWhenMiddlewareAbsent 记一条边界：
// 若某条装配路径漏了 ClientIPFromRemoteAddr，ClientIP 返回空串
// 而不是回退到 RemoteAddr。这是有意的 fail-closed 设计。
func TestClientIPMissingWhenMiddlewareAbsent(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	require.Equal(t, "", ClientIP(r),
		"未装ClientIPFromRemoteAddr 时应为空串，不得回退到 RemoteAddr")
	require.Equal(t, "192.0.2.1", PeerAddr(r))
}
