package harness

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestResolveOwner 钉 owner 的派生与 fail-closed 行为。
func TestResolveOwner(t *testing.T) {
	cases := []struct {
		name     string
		hostname string
		uid      int
		want     OwnerID
		wantErr  bool
	}{
		{name: "常规", hostname: "box", uid: 0, want: "box/0"},
		{name: "非 root", hostname: "box", uid: 1000, want: "box/1000"},
		{name: "带点与横线", hostname: "my-host.local", uid: 1, want: "my-host.local/1"},
		// 越出 label 字符集的字节被收进 [A-Za-z0-9._-]，其余替换成 _
		{name: "sanitize", hostname: "a b/c:d", uid: 2, want: "a_b_c_d/2"},
		// fail closed：归属是删除闸门，空 hostname 不能靠假值兜底
		{name: "空 hostname", hostname: "", uid: 0, wantErr: true},
		{name: "全空白 hostname", hostname: "   ", uid: 0, wantErr: true},
		{name: "sanitize 后为空", hostname: "///", uid: 0, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveOwner(c.hostname, c.uid)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 owner=%q", got)
				}
				if !IsKind(err, KindConfig) {
					t.Fatalf("期望 KindConfig，得到 %v", err)
				}
				if got != OwnerUnknown {
					t.Fatalf("报错时必须返回 OwnerUnknown，得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误：%v", err)
			}
			if got != c.want {
				t.Fatalf("owner = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestOwnerFingerprint 钉 owner 指纹的形状，以及 OwnerUnknown 不产生指纹。
func TestOwnerFingerprint(t *testing.T) {
	o, err := ResolveOwner("box", 0)
	if err != nil {
		t.Fatalf("非预期错误：%v", err)
	}
	fp := o.Fingerprint()
	if len(fp) != 8 {
		t.Fatalf("指纹应为 8 位，得到 %q", fp)
	}
	for _, c := range fp {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("指纹应为小写十六进制，得到 %q", fp)
		}
	}
	// 同输入必须同输出（它要进 iptables 注释，用来认「这条规则是不是我的」）
	again, _ := ResolveOwner("box", 0)
	if again.Fingerprint() != fp {
		t.Fatalf("同输入指纹不稳定：%q vs %q", again.Fingerprint(), fp)
	}
	// 不同 owner 必须不同指纹，否则「按注释删规则」会误伤
	other, _ := ResolveOwner("box", 1000)
	if other.Fingerprint() == fp {
		t.Fatalf("不同 owner 撞了同一个指纹 %q", fp)
	}
	if OwnerUnknown.Fingerprint() != "" {
		t.Fatalf("OwnerUnknown 不该有指纹，得到 %q", OwnerUnknown.Fingerprint())
	}
}

// TestResolveDockerEndpoint 钉 endpoint 规范化判据表。
func TestResolveDockerEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantID  string
		wantErr bool
	}{
		{name: "全空走默认", env: nil, wantID: DefaultDockerEndpointID},
		{name: "context 为 default", env: map[string]string{"DOCKER_CONTEXT": "default"}, wantID: DefaultDockerEndpointID},
		{name: "context 大小写不敏感", env: map[string]string{"DOCKER_CONTEXT": "Default"}, wantID: DefaultDockerEndpointID},
		{name: "显式本机 socket", env: map[string]string{"DOCKER_HOST": "unix:///run/docker.sock"}, wantID: "unix:///run/docker.sock"},
		{name: "另一条本机 socket 是另一个 daemon", env: map[string]string{"DOCKER_HOST": "unix:///tmp/other.sock"}, wantID: "unix:///tmp/other.sock"},
		{name: "scheme 大小写", env: map[string]string{"DOCKER_HOST": "UNIX:///run/docker.sock"}, wantID: "unix:///run/docker.sock"},

		{name: "tcp 拒绝", env: map[string]string{"DOCKER_HOST": "tcp://1.2.3.4:2375"}, wantErr: true},
		{name: "ssh 拒绝", env: map[string]string{"DOCKER_HOST": "ssh://host"}, wantErr: true},
		{name: "npipe 拒绝", env: map[string]string{"DOCKER_HOST": "npipe:////./pipe/docker"}, wantErr: true},
		{name: "fd 拒绝", env: map[string]string{"DOCKER_HOST": "fd://"}, wantErr: true},
		{name: "相对路径拒绝", env: map[string]string{"DOCKER_HOST": "unix://relative.sock"}, wantErr: true},
		{name: "unix 空路径拒绝", env: map[string]string{"DOCKER_HOST": "unix://"}, wantErr: true},
		{name: "无法解析拒绝", env: map[string]string{"DOCKER_HOST": "just-a-string"}, wantErr: true},
		// context 指向哪个 daemon 无法离线证明，而我们要用它当锁身份
		{name: "非默认 context 拒绝", env: map[string]string{"DOCKER_CONTEXT": "remote-build"}, wantErr: true},
		// DOCKER_HOST 非空时 context 被忽略（host 更具体）
		{name: "host 优先于 context", env: map[string]string{"DOCKER_HOST": "unix:///run/docker.sock", "DOCKER_CONTEXT": "weird"}, wantID: "unix:///run/docker.sock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ep, err := ResolveDockerEndpoint(env(c.env))
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝，得到 %q", ep.ID())
				}
				if !IsKind(err, KindConfig) {
					t.Fatalf("期望 KindConfig（部署配置错，重试无意义），得到 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误：%v", err)
			}
			if ep.ID() != c.wantID {
				t.Fatalf("ID = %q，期望 %q", ep.ID(), c.wantID)
			}
			if !ep.IsLocal() {
				t.Fatalf("解析成功的端点必须是本机的，得到 scheme=%q", ep.ID())
			}
		})
	}
}

// TestDockerEndpointRejectionHidesCredentials 是一条**公开面 canary**。
//
// `DOCKER_HOST` 可以是 `tcp://user:pass@host:2375`，而 `Error.Msg` 会进公开结果
// 与日志。拒绝消息里**只准出现 scheme**，绝不准回显原始取值——否则一次配置
// 错误就把凭据写进了公开面。
func TestDockerEndpointRejectionHidesCredentials(t *testing.T) {
	const secret = "s3cr3t-token"
	_, err := ResolveDockerEndpoint(env(map[string]string{
		"DOCKER_HOST": "tcp://user:" + secret + "@1.2.3.4:2375",
	}))
	if err == nil {
		t.Fatal("期望拒绝")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("拒绝消息回显了凭据：%q", msg)
	}
	if strings.Contains(msg, "1.2.3.4") {
		t.Fatalf("拒绝消息回显了远端地址：%q", msg)
	}
	// 但必须说清是哪种 scheme 被拒——否则用户无从下手
	if !strings.Contains(msg, "tcp") {
		t.Fatalf("拒绝消息应点明 scheme，得到 %q", msg)
	}
}

// TestDefaultDockerEndpointFingerprintIsStable 钉锁文件名的常量位。
//
// 默认部署下锁文件名是 `run-13c4025c.lock`。这个值变了，就意味着**升级会让
// 新旧版本的进程各自拿到一把锁**——正是这次要修的缺陷本身。所以它是一个
// 应当被钉死的常量，而不是一个碰巧算出来的数。
func TestDefaultDockerEndpointFingerprintIsStable(t *testing.T) {
	if got := DefaultDockerEndpoint().Fingerprint(); got != "13c4025c" {
		t.Fatalf("默认端点指纹 = %q，期望 13c4025c（改了它＝改了锁文件名＝新旧进程互不互斥）", got)
	}
	// 不同的本机 socket 必须落在不同的锁上：它们是不同的 daemon，
	// 不共享容器/网络/iptables，本就不该互斥
	a, _ := ResolveDockerEndpoint(env(map[string]string{"DOCKER_HOST": "unix:///run/docker.sock"}))
	b, _ := ResolveDockerEndpoint(env(map[string]string{"DOCKER_HOST": "unix:///tmp/other.sock"}))
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("两个不同的本机 socket 撞了同一个锁指纹")
	}
}

// TestChallengeIDFor 钉题目身份的唯一真源。
func TestChallengeIDFor(t *testing.T) {
	id, err := ChallengeIDFor("demo-1")
	if err != nil {
		t.Fatalf("非预期错误：%v", err)
	}
	if !ValidChallengeID(string(id)) {
		t.Fatalf("ChallengeIDFor 的产物必须过 ValidChallengeID，得到 %q", id)
	}
	if len(id) != 64 {
		t.Fatalf("长度应为 64（完整 sha256，不截断），得到 %d", len(id))
	}
	// 确定性：同一个 code 必须永远得到同一个 ID（产物路径与资源标签都靠它）
	again, _ := ChallengeIDFor("demo-1")
	if again != id {
		t.Fatalf("同 code 得到不同 ID：%q vs %q", id, again)
	}
	// 不同 code 必须不同 ID
	other, _ := ChallengeIDFor("demo-2")
	if other == id {
		t.Fatal("不同 code 撞了同一个 ID")
	}
	// 空/空白入口就拒绝，不让它变成 `runs/<id>//graph.json` 这种改掉布局的路径
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := ChallengeIDFor(bad); err == nil {
			t.Fatalf("空编号 %q 应被拒绝", bad)
		} else if !IsKind(err, KindConfig) {
			t.Fatalf("空编号应报 KindConfig，得到 %v", err)
		}
	}
}

// TestChallengeIDDoesNotNormalize 钉「不做 trim 后再哈希」。
//
// `" a "` 与 `"a"` 若是同一个 ID，宿主就替平台决定了「这两种写法是同一道题」。
// 平台真的下发过带空白的编号，而那到底是同一道题还是两道，不是宿主该猜的事。
func TestChallengeIDDoesNotNormalize(t *testing.T) {
	a, err := ChallengeIDFor("a")
	if err != nil {
		t.Fatalf("非预期错误：%v", err)
	}
	b, err := ChallengeIDFor(" a ")
	if err != nil {
		t.Fatalf("非预期错误：%v", err)
	}
	if a == b {
		t.Fatal("`\" a \"` 与 `\"a\"` 被判成了同一个题目身份——规范化是静默的，必须拒绝")
	}
}

// TestValidChallengeID 钉读取侧的严格校验：它挡的是路径穿越。
func TestValidChallengeID(t *testing.T) {
	valid, _ := ChallengeIDFor("demo-1")
	if !ValidChallengeID(string(valid)) {
		t.Fatalf("合法 ID 被判非法：%q", valid)
	}
	bad := []struct {
		name string
		in   string
	}{
		{"空", ""},
		{"太短", "abc"},
		{"太长", string(valid) + "0"},
		{"大写", strings.ToUpper(string(valid))},
		{"含路径穿越", "../../etc/passwd"},
		{"含斜杠", strings.Repeat("a", 62) + "/x"},
		{"非十六进制", strings.Repeat("z", 64)},
		{"含空白", strings.Repeat("a", 63) + " "},
	}
	for _, c := range bad {
		if ValidChallengeID(c.in) {
			t.Fatalf("%s：%q 不该被判为合法 ChallengeID", c.name, c.in)
		}
	}
}

// TestResolveExecutionIdentity 钉「一次解析、两处消费」。
func TestResolveExecutionIdentity(t *testing.T) {
	id, err := ResolveExecutionIdentity(env(nil), "box", 0)
	if err != nil {
		t.Fatalf("非预期错误：%v", err)
	}
	if id.Owner != "box/0" {
		t.Fatalf("owner = %q，期望 box/0", id.Owner)
	}
	if id.Endpoint.ID() != DefaultDockerEndpointID {
		t.Fatalf("endpoint = %q，期望默认端点", id.Endpoint.ID())
	}

	// 任一半失败即整体失败：不允许「锁按 A 端点、标签按空 owner」这种半成品身份
	if _, err := ResolveExecutionIdentity(env(map[string]string{"DOCKER_HOST": "tcp://1.2.3.4:2375"}), "box", 0); err == nil {
		t.Fatal("远程端点必须让整个身份解析失败")
	}
	if _, err := ResolveExecutionIdentity(env(nil), "", 0); err == nil {
		t.Fatal("空 hostname 必须让整个身份解析失败")
	}
}

// TestReclaimReportCounters 钉报告只暴露计数（公开面只放计数与指纹）。
func TestReclaimReportCounters(t *testing.T) {
	r := ReclaimReport{
		Reclaimed: []RunID{"r1", "r2"},
		Pending: []StaleObject{
			{RunID: "r3", Kind: "container", Owner: "other/0", Reason: StaleOwnerMismatch},
			{RunID: "", Kind: "network", Owner: OwnerUnknown, Reason: StaleUnparsable},
		},
	}
	if r.ReclaimedTotal() != 2 {
		t.Fatalf("ReclaimedTotal = %d，期望 2", r.ReclaimedTotal())
	}
	if r.PendingTotal() != 2 {
		t.Fatalf("PendingTotal = %d，期望 2", r.PendingTotal())
	}
	var zero ReclaimReport
	if zero.ReclaimedTotal() != 0 || zero.PendingTotal() != 0 {
		t.Fatal("零值报告应报 0")
	}
}
