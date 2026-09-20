package bridge

// 离线测试用的桥命令构造。
//
// 全部测试都用 mock SDK（`TSEC_MOCK=1` + `PYTHONPATH=bridge/testdata`）驱动
// 真实的 `bridge/bridge.py`——**不 mock Python 侧**。理由：桥的价值全在「Python
// 侧怎么把 SDK 的缺陷与契约翻成结构化结果」这一层，把它 mock 掉就等于把被测
// 对象换成了测试自己。

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testdataDir 返回 bridge/testdata 的绝对路径（runtime.Caller 而不是 cwd：
// `go test ./bridge/...` 的 cwd 是包目录，但这一点不该被依赖）。
func testdataDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	d := filepath.Join(filepath.Dir(file), "testdata")
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		t.Fatalf("找不到 testdata 目录: %s", d)
	}
	return d
}

// scriptPath 返回 bridge/bridge.py 的绝对路径。
func scriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	p := filepath.Join(filepath.Dir(file), "bridge.py")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("找不到 bridge.py: %s", p)
	}
	return p
}

// requirePython 在宿主没有可用的 python3 时跳过测试。
//
// **不 fail**：这台机器的宿主 python3 一定存在（桥的默认命令就是它），但
// 一个精简的 CI 镜像里可能没有。缺 python 是环境问题，不是被测代码的问题。
func requirePython(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("宿主没有 python3，跳过桥的离线测试")
	}
	return p
}

// testEnv 返回驱动 mock SDK 所需的环境变量。
//
// 凭据是**假值**：`BENCHMARK_TOKEN` 用 `test-token-not-real`，绝不用
// `/tmp/tsec/TsecBench-main/.agent.env` 里的那个（那是真 token，不得进任何
// 夹具、日志或提交）。mock SDK 不做任何网络 I/O，所以假值完全够用。
func testEnv(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{
		"TSEC_MOCK":           "1",
		"PYTHONPATH":          testdataDir(t),
		"BENCHMARK_BASE_URL":  "https://benchmark.invalid",
		"BENCHMARK_TOKEN":     "test-token-not-real",
		"TSEC_MOCK_SCENARIO":  "ok",
		"TSEC_MOCK_VPN_FAIL":  "",
		"TSEC_MOCK_SELF_KILL": "",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// mockConfig 造一个指向 mock SDK 的 ClientConfig。
func mockConfig(t *testing.T, extra map[string]string) ClientConfig {
	t.Helper()
	py := requirePython(t)
	return ClientConfig{
		Command: []string{py, scriptPath(t)},
		Timeout: 10 * time.Second,
		Env:     testEnv(t, extra),
	}
}

// newMockClient 起一个连着 mock SDK 的桥客户端，测试结束时关掉。
func newMockClient(t *testing.T, extra map[string]string) *Client {
	t.Helper()
	c, err := NewClient(mockConfig(t, extra))
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}
	t.Cleanup(c.Shutdown)
	return c
}

// mustNotLeak 断言一段文本里不含凭据。
//
// 这是安全断言，不是形式主义：桥的错误消息会进 JSON（看板、报告、日志），
// 所以「token 有没有漏进错误文本」必须被钉住。
func mustNotLeak(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range []string{"test-token-not-real", "BENCHMARK_TOKEN="} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s 泄漏了凭据片段 %q: %s", what, secret, text)
		}
	}
}
