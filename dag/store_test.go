package dag

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// ── 往返 ──

// Save → Load 之后图必须**等价**（不是「差不多」）。断点续跑完全依赖这条：
// 少一条边或多一个 abandoned 状态，下一轮调度就会做不同的事。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")

	g := newTestGraph(t, "10.0.0.1:80")
	i1 := mustIntent(t, g, IntentRecon, "侦察：扫端口")
	i2 := mustIntent(t, g, IntentExploit, "利用 /upload")
	f1 := mustFact(t, g, FactService, "nginx/1.18.0", "bash: whatweb http://10.0.0.1")
	f2 := mustFact(t, g, FactFoothold, "www-data shell via upload", "bash: curl -F file=@x.php")
	cred := mustFact(t, g, FactCredential, "admin:Admin@123", "bash: curl .../login")

	if err := g.Activate(i1); err != nil {
		t.Fatal(err)
	}
	if err := g.Settle(i1, []string{f1}, harness.RoundResult{}); err != nil {
		t.Fatal(err)
	}
	_ = g.Link(i2, f2, EdgeProduces, 2)
	_ = g.Require(i2, cred)
	if _, err := g.EnableFrom(f1, Node{Kind: NodeIntent, IntentKind: IntentExploit, Goal: "试 /admin 弱口令"}); err != nil {
		t.Fatal(err)
	}
	negID, err := g.AddNegative(i1, "22 端口 SSH 未开放", "bash: nmap", TrustHost, "call_9")
	if err != nil {
		t.Fatal(err)
	}
	g.Round = 5
	if err := g.Save(path); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := g.Equivalent(back); err != nil {
		t.Fatalf("往返后图不等价: %v", err)
	}
	// 关键字段逐个复查（Equivalent 只看语义键，这里确认字段级还原）
	if n := back.Node(i1); n == nil || n.State != IntentDone || n.Attempts != 1 {
		t.Errorf("意图状态/尝试次数未还原: %+v", n)
	}
	if n := back.Node(negID); n == nil || n.Refutes != i1 {
		t.Errorf("negative 的 refutes 未还原: %+v", n)
	}
	if n := back.Node(f2); n == nil || n.ToolCallID != "call_x" || n.Trust != TrustHost {
		t.Errorf("证据引用/信任层未还原: %+v", n)
	}
	if got := back.Produced(i1); len(got) != 1 || got[0] != f1 {
		t.Errorf("produces 边未还原: %v", got)
	}
	if got := back.RefutedBy(i1); len(got) != 1 {
		t.Errorf("refutes 边未还原: %v", got)
	}
	if back.Round != 5 {
		t.Errorf("轮次未还原: %d", back.Round)
	}
	if back.Category != "pentest" || back.Code != "web-01" {
		t.Errorf("题目身份未还原: %+v", back)
	}
	// 索引必须重建（否则去重静默失效）
	if _, err := back.AddFact(Node{Kind: NodeFact, FactKind: FactService,
		Content: "NGINX/1.18.0", Source: "bash: x"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("载入后去重索引应生效, got %v", err)
	}
	// 续跑：还能继续加节点且 id 不撞车
	nf, err := back.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "/new/path.php", Source: "bash: x"})
	if err != nil {
		t.Fatalf("续跑写入失败: %v", err)
	}
	for _, n := range back.Nodes() {
		if n.ID == nf && n.Seq <= 0 {
			t.Error("新节点的 Seq 应大于所有已载入节点")
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("文件不存在应可判 os.ErrNotExist, got %v", err)
	}
	// LoadOrNew 在文件不存在时新建
	ch := harness.Challenge{Code: "c9", Category: "crypto", Addrs: []string{"1.2.3.4:9"}}
	g, err := LoadOrNew(filepath.Join(t.TempDir(), "nope.json"), ch)
	if err != nil {
		t.Fatalf("LoadOrNew 应新建: %v", err)
	}
	if g.Code != "c9" || len(g.Facts(FactTarget)) != 1 {
		t.Errorf("新建的图应带题目初始化: %+v", g.Stats())
	}
}

// 载入失败**不能**静默新建一张空图——那等于把已积累的事实全部丢掉，
// 而且表现为「这道题从头开始跑」，极难察觉。
func TestLoadOrNewDoesNotSwallowCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrNew(path, harness.Challenge{Code: "c1"}); err == nil {
		t.Fatal("损坏的图必须报错，不能悄悄新建")
	}
}

// ── schema 版本与迁移 ──

func TestSchemaVersionWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	g := newTestGraph(t, "10.0.0.1:80")
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc["schema"].(float64); !ok || int(v) != SchemaVersion {
		t.Errorf("落盘必须带 schema 版本 %d, got %v", SchemaVersion, doc["schema"])
	}
}

// v0 文档（无 schema 字段）必须能载入，且三件事都要做：
// 补 shape、补 trust、**净化存量噪音凭证**（前身 `_load` 里那段过滤的移植）。
func TestMigrateFromV0(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	// 手工造一份 v0 文档：无 schema、无 shape、凭证里混着 B14 噪音、
	// negative 没有 refutes（老模型里没这个概念）。
	v0 := `{
	  "code": "old-01",
	  "category": "pentest",
	  "nodes": [
	    {"id":"f1","kind":"fact","factKind":"target","content":"10.0.0.1:80","source":"platform"},
	    {"id":"f2","kind":"fact","factKind":"credential","content":"login: 500","source":"bash: curl"},
	    {"id":"f3","kind":"fact","factKind":"credential","content":"admin:Admin@123","source":"bash: curl"},
	    {"id":"f4","kind":"fact","factKind":"credential","content":"passwd: HTTPConnectionPool(host='10.x.x.x',","source":"bash: x"},
	    {"id":"f5","kind":"fact","factKind":"negative","content":"22 端口未开放","source":"bash: nmap"},
	    {"id":"i1","kind":"intent","intentKind":"recon","goal":"扫端口","state":"pending"}
	  ],
	  "edges": [{"from":"i1","to":"f1","kind":"requires","round":1}]
	}`
	if err := os.WriteFile(path, []byte(v0), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := Load(path)
	if err != nil {
		t.Fatalf("v0 文档必须能载入（迁移路径）: %v", err)
	}
	// 噪音凭证被剔除，真凭证保留
	creds := g.Facts(FactCredential)
	if len(creds) != 1 || creds[0].Content != "admin:Admin@123" {
		t.Errorf("迁移应剔除噪音凭证、保留真凭证, got %+v", creds)
	}
	// 老节点补上 trust
	for _, f := range g.Facts("") {
		if f.Trust == "" {
			t.Errorf("迁移后每条事实都应有信任层: %+v", f)
		}
	}
	// shape 被补上（否则「答案形状拒入图」彻底失效）
	if g.Shape.Empty() {
		t.Error("迁移必须补 shape")
	}
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "flag{x}", Source: "bash: cat f"}); !errors.Is(err, ErrAnswerShaped) {
		t.Errorf("迁移后答案形状判定必须生效, got %v", err)
	}
	// 无 refutes 的老 negative 降级为 artifact（它无法剪枝任何东西）
	if len(g.Negative()) != 0 {
		t.Errorf("无 refutes 的老 negative 应降级, got %d 条", len(g.Negative()))
	}
	if g.Node("f5") == nil {
		t.Error("降级不是删除：节点应保留（信息不丢）")
	}
}

// 未来版本必须**拒绝**载入，而不是猜着读（猜着读会静默丢字段）。
func TestLoadRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	doc := `{"schema": 99, "code":"x", "nodes":[], "edges":[]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("更高版本的 schema 必须报错")
	}
}

// ── flag 明文不得落盘 ──

// 前身事故：`MEMORY.md` / `_blackboard.json` 会带上 flag 原文。这里在落盘口
// 再洗一遍——图里本来不该有答案（不变量 2），但**描述性文本里可能出现**
// （agent 在 goal 或 negative 内容里写过 flag）。
func TestNoFlagPlaintextOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	g := newTestGraph(t)

	// 走 AddIntent：goal **不拒收**答案形状（猜测是正常行为），但入库口会把它
	// 净化成指纹——所以这里要断言的正是「明文从进图那一刻就不存在」，落盘口只是
	// 最后一道防线（描述性文本里仍可能有明文，见 TestRawScrubbedOnSave）。
	secret := "flag{s3cr3t_value_here}"
	iid, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "复核候选 " + secret + " 是否成立"})
	if err != nil {
		t.Fatalf("意图应能入库: %v", err)
	}
	if got := g.Node(iid).Goal; strings.Contains(got, secret) || strings.Contains(got, "s3cr3t") {
		t.Fatalf("入库的意图目标里不得有明文: %q", got)
	}
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), secret) {
		t.Fatalf("flag 明文不得落盘:\n%s", string(b))
	}
	if strings.Contains(string(b), "s3cr3t") {
		t.Fatal("flag 载荷的任何片段都不得落盘")
	}
	// 但指纹要在（否则「我是不是又想到同一个」无从判断）
	if !strings.Contains(string(b), "fp:") {
		t.Error("落盘的应是指纹形式")
	}
}

// 落盘是原子的：写临时文件再 rename。这样进程在落盘中途被杀，读者要么看到
// 旧版、要么看到新版，不会读到半个 JSON。
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	g := newTestGraph(t, "10.0.0.1:80")
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Error("落盘后不该残留 .tmp 文件")
	}
	// 目录不存在时应自动建
	nested := filepath.Join(dir, "runs", "web-01", "dag.json")
	if err := g.Save(nested); err != nil {
		t.Fatalf("应自动建目录: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("嵌套路径应写入成功: %v", err)
	}
	// 覆写不损坏：连写两次后仍可载入
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("覆写后应仍可载入: %v", err)
	}
}

// 被拒写入的审计也要落盘——「为什么这条事实没进图」必须可回答。
func TestRejectionsPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	g := newTestGraph(t)
	_, _ = g.AddFact(Node{Kind: NodeFact, FactKind: FactService, Content: "x"}) // 无来源
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rs := back.Rejections()
	if len(rs) != 1 || !strings.Contains(rs[0].Reason, "来源") {
		t.Errorf("被拒审计应落盘且带原因, got %+v", rs)
	}
}

// json.Marshal / Unmarshal 也走同一条迁移 + 索引重建路径，
// 避免出现「索引为空的图」这种静默失效。
func TestJSONMarshalRoundTrip(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	i := mustIntent(t, g, IntentRecon, "侦察")
	_ = mustFact(t, g, FactService, "nginx", "bash: x")
	_, _ = g.AddNegative(i, "22 未开放", "bash: nmap", TrustHost, "c1")

	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var back Graph
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if err := g.Equivalent(&back); err != nil {
		t.Fatalf("JSON 往返后不等价: %v", err)
	}
	if _, err := back.AddFact(Node{Kind: NodeFact, FactKind: FactService,
		Content: "nginx", Source: "bash: x"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("JSON 载入后去重索引应生效, got %v", err)
	}
}

// Raw 是工具输出的原文摘录（取证用，不参与判定）——「agent 把 flag 写进文件再
// cat 出来」时 flag 就在 Raw 里。它最容易被当成「无所谓的一小段」放过，所以
// 落盘口必须同样洗一遍。
//
// **这条测试以前带 `t.Skip`**（「本次抽样的 Raw 片段没覆盖到 flag 处，换一条语料
// 即可」）——那是一处不能接受的让步：擦洗是**安全性质**，「这次没测到就算了」等于
// 让整条断言在语料一改之后就静默失效，而它失效时不会有人知道（测试是绿的）。
// 所以语料改成**构造性保证覆盖**：三条来源各写一遍明文，且断言的判据是「明文不
// 出现」而不是「抽到了含明文的 Raw」。
//
// 三条来源分别对应三个真实的泄漏面：
//  1. 被拒审计（Rejection.Content）——「为什么这条没进图」的记录里带着原文；
//  2. 普通事实的 Raw——工具输出的原文摘录；
//  3. 意图目标——agent 自己的散文（AddIntent 会净化信封形态，但裸串形态的残留
//     只能靠落盘口兜底）。
func TestRawScrubbedOnSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	g := newTestGraph(t)
	const secret = "flag{plaintext_must_not_hit_disk}"
	const fragment = "plaintext_must_not_hit_disk"

	// 1) 一条会被**拒收**的答案形状事实：拒收记录里会带上内容原文。
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: secret, Source: "bash: cat /tmp/f", ToolCallID: "c1"}); err == nil {
		t.Fatal("答案形状的事实必须被拒（前置条件：这条语料要产生拒收记录）")
	}
	// 2) 一条普通事实，其 Raw 片段里嵌了明文（模拟工具输出里带 flag 的原文）
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "/tmp/leak.txt", Raw: "cat /tmp/leak.txt → " + secret + "\n",
		Source: "bash: cat /tmp/leak.txt", ToolCallID: "c2"}); err != nil {
		t.Fatalf("普通事实应能入图: %v", err)
	}
	// 3) 一个意图，目标里带明文（裸串形态：信封形态在入库口就被净化了，
	//    这里刻意用一段**散文里嵌明文**的文本，走落盘口的兜底路径）
	if _, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "确认 " + secret + " 是否来自目标主机"}); err != nil {
		t.Fatalf("意图应能入库: %v", err)
	}

	// 前置条件：Raw 里确实有明文（否则下面的断言可能是在空转）
	hasRaw := false
	for _, n := range g.Facts("") {
		if strings.Contains(n.Raw, secret) {
			hasRaw = true
		}
	}
	if !hasRaw {
		t.Fatal("语料没覆盖到 Raw 里的明文（前置条件不成立，测试会空转）")
	}

	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	blob := string(b)
	if strings.Contains(blob, secret) || strings.Contains(blob, fragment) {
		t.Fatalf("flag 明文不得落盘:\n%s", blob)
	}
	// 指纹要在（否则「我是不是又想到同一个」无从判断）
	if !strings.Contains(blob, FlagFingerprint(secret)) {
		t.Errorf("明文应被替换为指纹 %s", FlagFingerprint(secret))
	}
	// 擦洗不能把图弄坏：载回来仍能过不变量复查
	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败（擦洗把图弄坏了）: %v", err)
	}
	if errs := back.Validate(); len(errs) != 0 {
		t.Errorf("载回后不变量校验失败: %v", errs)
	}
}
