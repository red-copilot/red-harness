package bridge

// 桥命令可覆盖性 + doctor 探针的测试。
//
// 这两件事在本机是**硬需求**：SDK 装在容器层、面向 python3.14，而宿主
// python3.12 没有 httpx，所以 `import tsec_benchmark` 在宿主上必然失败。
// 本机真跑的唯一路径是把桥命令覆盖成 `docker exec <容器> python3 -m bridge`。
// 于是 doctor 必须能如实报出「宿主导入失败但容器可用」这种**正常配置**，
// 而不是笼统报「缺 SDK」。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件测两件事，都是**本机硬需求**：桥命令必须可覆盖，doctor 必须能如实
// 报出「宿主导入失败但容器可用」这种**正常配置**。
//
// 为什么是硬需求：SDK 装在容器层、面向 python3.14，而宿主 python3.12 没有
// httpx，所以 `import tsec_benchmark` 在宿主上必然失败。本机真跑的唯一路径
// 是把桥命令覆盖成 `docker exec <容器> python3 -m bridge`。

// TestDefaultCommandFindsScript 钉住默认命令能定位到 bridge/bridge.py。
//
// 搜索顺序是「cwd 相对 → 可执行文件相对」：CLI 通常从仓库根跑，而 `go test`
// 从包目录跑，两者都要能找到。
func TestDefaultCommandFindsScript(t *testing.T) {
	// 从包目录跑时 cwd 是 bridge/，所以要靠可执行文件相对路径找到。
	argv, err := DefaultCommand()
	if err != nil {
		t.Fatalf("DefaultCommand 失败: %v", err)
	}
	if len(argv) != 2 {
		t.Fatalf("默认命令应为两个词，实际 %v", argv)
	}
	if argv[0] != "python3" && !strings.HasSuffix(argv[0], "python3") {
		t.Errorf("默认解释器应为 python3，实际 %q", argv[0])
	}
	if !strings.HasSuffix(argv[1], "bridge.py") {
		t.Errorf("默认脚本应为 bridge.py，实际 %q", argv[1])
	}
	if _, err := os.Stat(argv[1]); err != nil {
		t.Errorf("默认脚本不存在: %v", err)
	}
}

// TestEnvOverridesCommand 钉住 **桥命令可覆盖**（本机硬需求）。
//
// 失效模式：桥命令写死在代码里，于是「宿主没有 httpx」变成一个无法绕过的
// 死结——而这台机器上容器才是唯一可行的执行环境。
func TestEnvOverridesCommand(t *testing.T) {
	t.Setenv(EnvBridgeCmd, "docker exec bench-sdk python3 -m bridge")

	argv, err := DefaultCommand()
	if err != nil {
		t.Fatalf("DefaultCommand 失败: %v", err)
	}
	want := []string{"docker", "exec", "bench-sdk", "python3", "-m", "bridge"}
	if len(argv) != len(want) {
		t.Fatalf("覆盖命令解析错: %v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("覆盖命令第 %d 个词应为 %q，实际 %q", i, want[i], argv[i])
		}
	}
}

// TestEnvOverrideTakesEffectInClient 钉住覆盖真的被 Client 用上。
//
// 用一个「会把收到的 argv 写进文件」的假命令验证：Client 必须走覆盖值，
// 而不是回落默认脚本。假桥同时照抄真实桥的两个关键行为——先打握手行、
// 把 sys.stdout 换成 stderr。
func TestEnvOverrideTakesEffectInClient(t *testing.T) {
	py := requirePython(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	script := filepath.Join(dir, "fake_bridge.py")
	body := `import sys, json
with open(` + pyQuote(argvFile) + `, "w") as f:
    f.write(json.dumps(sys.argv))
out = sys.stdout
sys.stdout = sys.stderr
out.write(json.dumps({"event": "hello", "sdk": "0.1.2", "python": "3.12.0"}) + "\n")
out.flush()
for line in sys.stdin:
    req = json.loads(line)
    out.write(json.dumps({"id": req.get("id"), "ok": True, "result": []}) + "\n")
    out.flush()
`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBridgeCmd, py+" "+script)

	c, err := NewClient(ClientConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewClient 走覆盖命令失败: %v", err)
	}
	defer c.Shutdown()

	b, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("假桥没有写出 argv: %v", err)
	}
	if !strings.Contains(string(b), "fake_bridge.py") {
		t.Errorf("Client 没有用环境变量里的覆盖命令: %s", b)
	}
	// 覆盖命令的 SDK 版本应被握手记录下来（doctor 依赖它）。
	if got := c.Handshake().SDK; got != "0.1.2" {
		t.Errorf("握手 SDK 版本应为 0.1.2，实际 %q", got)
	}
}

// TestProbeReportsHostFailureButBridgeOK 钉住 doctor 需要的两级结论。
//
// **这是本任务里最容易被做错的一条**：「宿主导入失败 + 桥侧可用」是**正常
// 配置**（SDK 装在容器层），必须报成通过并附说明，而不是「缺 SDK」。
// 反过来，宿主成功而桥失败才是真故障。
func TestProbeReportsHostFailureButBridgeOK(t *testing.T) {
	cfg := mockConfig(t, nil)
	// 让一级探测（宿主 python）必然失败：给它一个不存在的解释器路径。
	// 注意 Probe 只在 argv[0] 不是 docker 时才把它当宿主 python 用。
	cfg.Command = []string{requirePython(t), scriptPath(t)}
	res := Probe(context.Background(), cfg)

	if res.HostImportOK {
		t.Skip("这台机器的宿主 python3 能导入 tsec_benchmark，跳过组合断言")
	}
	if !res.BridgeOK {
		t.Fatalf("桥侧应可用（mock SDK 在 PYTHONPATH 里）: %+v", res)
	}
	if res.SDKVersion != "0.1.2" {
		t.Errorf("桥侧 SDK 版本应为 0.1.2，实际 %q", res.SDKVersion)
	}
	// Detail 必须把「宿主失败是正常配置」说出来，而不是让人以为缺依赖。
	if !strings.Contains(res.Detail, "正常配置") {
		t.Errorf("Detail 应说明宿主失败是正常配置: %q", res.Detail)
	}
	mustNotLeak(t, "Probe.Detail", res.Detail)
}

// TestProbeReportsBridgeFailure 钉住「宿主成功但桥失败」这种真故障被报出来。
func TestProbeReportsBridgeFailure(t *testing.T) {
	cfg := mockConfig(t, nil)
	cfg.Command = []string{"/nonexistent/bridge-not-here"}
	res := Probe(context.Background(), cfg)
	if res.BridgeOK {
		t.Fatal("桥命令不存在时 BridgeOK 应为假")
	}
	if !strings.Contains(res.Detail, "桥命令起不来") {
		t.Errorf("Detail 应指出桥命令起不来: %q", res.Detail)
	}
	// 桥起不来时 SDK 版本必须是空的——doctor 不能因为「桥失败了」就编一个版本。
	if res.SDKVersion != "" || res.PythonVersion != "" {
		t.Errorf("桥起不来时不该有版本信息: %+v", res)
	}
	mustNotLeak(t, "Probe.Detail(失败路径)", res.Detail)
}

// pyQuote 把一个路径渲染成 Python 字符串字面量（测试夹具用，路径来自 t.TempDir）。
func pyQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}
