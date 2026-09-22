package piai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	body := `# 注释行
export FOO=bar
QUOTED="a b c"
SINGLE='x=y'
EMPTY=
NOEQUALS
  SPACED  =  v  # 不是注释（行内 # 不剥离）
WITH_HASH="#keep"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := parseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"FOO":       "bar",
		"QUOTED":    "a b c",
		"SINGLE":    "x=y",
		"EMPTY":     "",
		"SPACED":    "v  # 不是注释（行内 # 不剥离）",
		"WITH_HASH": "#keep",
	}
	for k, v := range want {
		if got := m[k]; got != v {
			t.Errorf("%s = %q, 期望 %q", k, got, v)
		}
	}
	if _, ok := m["NOEQUALS"]; ok {
		t.Error("无 = 的行不应产生键")
	}
}

// TestLoadEnvWalksUp 锁住「向上逐级查找」这条行为：`go run ./example` 与
// `./cmd/redcopilot` 的工作目录不同，固定读仓库根会静默漏掉凭据（pi 拿不到
// key 就退回默认 provider，401 被呈现成空会话 ⇒ 整库静默烧掉）。
func TestLoadEnvWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("K=root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	m, path, err := LoadEnv(deep)
	if err != nil {
		t.Fatal(err)
	}
	if m["K"] != "root" {
		t.Fatalf("向上查找失败: %v", m)
	}
	if path != filepath.Join(root, ".env") {
		t.Fatalf("path = %s", path)
	}
}

// TestLoadEnvNearestWins：找到第一个就停，不能继续向上覆盖成更外层的。
func TestLoadEnvNearestWins(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".env"), []byte("K=outer\n"), 0o600)
	os.WriteFile(filepath.Join(sub, ".env"), []byte("K=inner\n"), 0o600)
	m, _, err := LoadEnv(sub)
	if err != nil {
		t.Fatal(err)
	}
	if m["K"] != "inner" {
		t.Fatalf("最近优先失效: %v", m)
	}
}

// TestLoadEnvMissingIsNotError：「没有 .env」是合法状态（可能全靠进程环境变量），
// 必须返回空 map 而不是错误——否则本机离线测试与 example 都跑不起来。
func TestLoadEnvMissingIsNotError(t *testing.T) {
	m, path, err := LoadEnv(t.TempDir())
	if err != nil {
		t.Fatalf("缺 .env 不应报错: %v", err)
	}
	if len(m) != 0 || path != "" {
		t.Fatalf("m=%v path=%q", m, path)
	}
}

func TestResolveEnvFileExplicitMustExist(t *testing.T) {
	if _, _, err := resolveEnvFile(filepath.Join(t.TempDir(), "nope.env"), t.TempDir()); err == nil {
		t.Fatal("显式指定的 .env 不存在时必须报错（显式配置静默降级是事故温床）")
	}
}

func TestSandboxEnvExcludesPlatformToken(t *testing.T) {
	got := sandboxEnv(map[string]string{
		"OPENCODE_API_KEY": "test-provider-canary",
		"BENCHMARK_TOKEN":  "test-platform-canary",
		"PATH":             "/host-only",
	}, "opencode-go", "/work/.home")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "test-provider-canary") {
		t.Fatal("provider credential did not reach sandbox")
	}
	if strings.Contains(joined, "test-platform-canary") || strings.Contains(joined, "/host-only") {
		t.Fatal("host-only environment reached sandbox")
	}
}

// TestChildEnvPathPrepended：pi 是 `#!/usr/bin/env node` 脚本，系统 node 是 v18
// （跑不起来），必须把 bundled node 所在目录前置进 PATH，否则 pi 起不来。
func TestChildEnvPathPrepended(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := childEnv(map[string]string{"A": "1", "PATH": "/should/be/overridden"}, "/opt/pinode/bin")
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := cut(kv, "=")
		got[k] = v
	}
	if got["A"] != "1" {
		t.Errorf("A = %q", got["A"])
	}
	if got["PATH"] != "/opt/pinode/bin:/usr/bin" {
		t.Errorf("PATH = %q，期望 bundled 目录前置且进程环境覆盖 .env", got["PATH"])
	}
}

// TestChildEnvProcessEnvWins：CLI flag > 环境变量 > .env。环境变量覆盖 .env
// 是配置优先级的实现点。
func TestChildEnvProcessEnvWins(t *testing.T) {
	t.Setenv("SAME", "from-process")
	env := childEnv(map[string]string{"SAME": "from-dotenv"}, "")
	for _, kv := range env {
		if k, v, _ := cut(kv, "="); k == "SAME" && v != "from-process" {
			t.Fatalf("SAME = %q，期望进程环境胜出", v)
		}
	}
}

func cut(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}
