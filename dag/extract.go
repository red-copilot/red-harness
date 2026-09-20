package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// ── 抽取器 ──
//
// 这是「宿主侧抽取」这条通道（设计文档 §一「事实从哪来」的第 2 条）。它的价值是
// **独立于 agent 的判断**：它抓住 agent 没申报的东西，也可以作为 report_fact 的
// 对账（agent 申报了但工具输出里没有 ⇒ 该降置信度）。
//
// 前身的教训（必须逐条落地）：
//
//  1. **不要截断输入**。前身 `blackboard.py:236/245/252` 把输出切到 2000/3000
//     字符再抽，注释里写着这是「防噪音」，实际副作用是**切掉了长输出里的真事实**
//     ——nmap -sV 扫 65535 个端口、ffuf 的几万行结果，真信号全在后半段。
//     本实现抽的是**完整输出**，落盘的是 sha256[:12] + 命中偏移。既不掉真事实，
//     也不把大盘文本灌进图（图里只存规范化后的短内容）。
//  2. **凭证必须过 B14 质量闸**。前身某题攒了 61 条垃圾凭证事实
//     （`login ==`、`admin: 500`、`passwd: HTTPConnectionPool(host='10.x.x.x',`），
//     注入下一场时 actionable_assets 只取前 5 条，噪音把真信号挤没。
//  3. **flag 候选永不入事实库**。前身 blackboard.py:260 的注释就是这条。本实现
//     把它升级成机制：Extract 抽完每条都过 answerShaped，命中即丢（它该去 gate）。

// maxExtractPerKind 是**每个工具调用、每类事实**的抽取上限。
//
// 为什么需要上限：`nmap -p- -sV` 一次能报 65535 个端口，全部入图会把「已知事实」
// 段撑爆，渲染进 prompt 时反而淹掉真信号（这正是前身 61 条垃圾凭证的失败模式，
// 只不过换成端口）。截断在这里是**保留**——我们留的是前 N 条，而不是随机切一段。
const maxExtractPerKind = 50

// maxRawSnippet 是 Raw 取证片段的长度上限。
//
// 注意这与「不截断输入」不冲突：**抽取**用的是完整输出，Raw 只是给人看的取证
// 片段（报告/态势台点击节点时显示）。真正记录「来自完整输出」这件事的是
// Fingerprint + Offset + OutputLen 三个字段。
const maxRawSnippet = 160

var (
	// ipPortRe 抓 IP[:端口]。逐条移植前身 `_IP_PORT_RX`，但加了八位组范围校验——
	// 前身那条 `\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}` 会把 `1.2.3.4.5.6.7.8`（版本号
	// 串、时间戳串）也吞进来，而 `999.999.999.999` 这种明显不是地址。
	ipPortRe = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(?::(\d{1,5}))?\b`)

	// serviceRe 抓服务名。移植前身 `_SERVICE_RX`，补齐常见服务（前身只有 11 个）。
	serviceRe = regexp.MustCompile(`(?i)\b(http|https|ssh|ftp|sftp|mysql|mariadb|redis|` +
		`smtp|dns|smb|netbios|rdp|vnc|mssql|postgres(?:ql)?|mongodb|elasticsearch|` +
		`telnet|ldap|kerberos|nfs|docker|kubernetes|tomcat|nginx|apache|iis|php|` +
		`node\.?js|flask|django|spring|jenkins|gitlab|grafana|zabbix|wordpress|` +
		`joomla|drupal|weblogic|struts|shiro|fastjson|log4j)\b`)

	// versionRe 抓「服务 版本号」形态，例如 `nginx/1.18.0`、`OpenSSH 8.2p1`、
	// `PHP 7.4.3`、`Apache/2.4.41 (Ubuntu)`。版本号是漏洞判定的关键输入，前身
	// 只抓了服务名，于是「nginx 1.18 有某个 CVE」这类判断无从谈起。
	versionRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9_.+\-]{1,24})[/ ]v?(\d+(?:\.\d+){1,3}[a-z0-9._\-]*)`)

	// ── 凭证（B14 逐条移植）──
	//
	// 前身 `_CRED_RX` 的注释记录了事故：旧正则用 `\S+` 取值，把 HTML 表单字段
	// （`login ==`）、SQLi payload（`password='`）、HTTP 状态码（`admin: 500`）
	// 统统当凭证。收紧的三点：
	//   - 值必须是引号串或 3~64 位的字母数字/常见符号串；
	//   - 分隔符只用空格/制表符（`\s` 含换行，会把 `root:\ncss` 拼成一条凭证）；
	//   - 键前要求「不是字母数字」挡掉 `bypass:`/`username:` 这类词内误匹配，但放行
	//     `my_password=` 这种下划线前缀键。**Go 的 RE2 没有后顾断言**，所以用
	//     「消耗一个非字母数字字符 + 捕获组」表达（见 credRe 的写法与 Extract 里的
	//     group 1 取值）——这条前身用 Python 的 `(?<!...)` 一行搞定，移植时必须改写，
	//     照抄会让包在 init 阶段 panic。
	credRe = regexp.MustCompile(`(?i)(?:[^A-Za-z0-9]|^)((?:user|login|admin|root|password|passwd|pwd|pass)[ \t]*[:=][ \t]*` +
		`(?:"([^"\n]{3,64})"|'([^'\n]{3,64})'|([A-Za-z0-9!@#$%^&*_\-\.]{3,64})))`)

	// credFullRe 是「整条内容必须键=值到底」的复核闸（前身 `_CRED_FULL_RX`）。
	// 为什么需要它：credRe 是**前缀匹配**，历史数据里
	// `passwd: HTTPConnectionPool(host='10.x.x.x',` 会取到 HTTPConnectionPool 而
	// 通过初判。要求全串匹配后，尾部残留（括号/引号/逗号）即判噪音。允许
	// markdown 加粗的尾部 `**`（工具输出里常见）。
	credFullRe = regexp.MustCompile(`(?i)^[ \t]*(?:user|login|admin|root|password|passwd|pwd|pass)` +
		`[ \t]*[:=][ \t]*` +
		`(?:"([^"\n]{3,64})"|'([^'\n]{3,64})'|([A-Za-z0-9!@#$%^&*_\-\.]{3,64}))` +
		`[ \t]*\**[ \t]*$`)

	// credPlaceholders 是占位符 / 常见爆破 payload（前身 `_CRED_PLACEHOLDERS` 逐条
	// 移植）。作为「已发现凭证」注入只会误导下一场。
	credPlaceholders = map[string]bool{
		"test": true, "testing": true, "tests": true, "guest": true, "zzz": true,
		"wrongpass": true, "example": true, "changeme": true, "foo": true, "bar": true,
		"baz": true, "asdf": true, "qwerty": true, "123456": true, "letmein": true,
		"hello": true, "world": true, "password": true, "passwd": true, "pwd": true,
		"pass": true, "secret": true, "placeholder": true, "yourpassword": true,
		"your_password": true, "xxx": true, "abcd": true, "abcd1234": true,
		"none": true, "null": true, "undefined": true, "todo": true, "tbd": true,
		"string": true, "value": true, "demo": true, "sample": true, "flag": true,
		"admin": true, "root": true, "user": true, "login": true,
	}

	// artifactRe 抓文件路径。这是前身完全没有的一类（它的 kind 只有
	// network/service/credential/vuln/foothold），而 CTF 里「读源码」是最常见的
	// 第一步——路径本身就是攻击面（`/upload`、`/api/v1/users`）。
	artifactRe = regexp.MustCompile(`(?:^|[\s"'(\[])(/[A-Za-z0-9_\-./]{2,120}\.(?:php|py|js|ts|rb|go|java|jar|war|sh|pl|cgi|conf|cnf|ini|yml|yaml|json|xml|sql|db|sqlite|pem|key|crt|zip|tar|gz|bak|old|txt|md|log|bin|elf|so|dll|exe))`)
	// urlPathRe 抓 URL 路径（`http://host/admin` 的 `/admin`）。
	urlPathRe = regexp.MustCompile(`(?i)https?://[^\s"'<>]+?(/[A-Za-z0-9_\-./?=&%]{1,120})`)
)

// Extract 从一次工具调用的事件里抽事实。round 用于给节点打轮次戳。
//
// **三层信任的落地**：
//   - 这里产出的一律是 host-verified（宿主从原始输出指纹匹配得到），Trust 显式
//     标 TrustHost、Source 记 `tool: cmd`。
//   - 输入是**完整输出**（ev.Output 从不截断），落盘只有指纹 + 偏移。
//   - 命中的内容再过一次 answerShaped：flag 候选不进图（它属于 gate）。
//
// 返回的节点**未入库**，调用方逐条 AddFact 并处理 error（重复/违规）。这样做的
// 原因：抽取器不该知道图的状态，而 AddFact 是唯一的校验入口——把校验放在抽取器
// 里就又多了一处「同一判定散落多处」（前身 B55 的教训）。
//
// ── 两条通道的**前置条件不同**（这是一个真实事故的修法）──
//
// 正则抽取（1-4 类）读的是 ev.Output，没有输出就没有可抽的东西；但 report_fact
// 申报（第 5 类）读的是 ev.Details，与 Output **无关**。原实现把两者一起关在
// `ev.Output == ""` 这道门后面，于是「纯申报调用」（宿主扩展只回一个
// `{facts: [...], next: "..."}`、没有 stdout）里的事实被**整条静默丢弃**——
// 而 report_fact 恰恰是最可能以纯申报形态发出的通道（agent 说「我知道什么」，
// 本来就不需要跑命令）。M7 的宿主扩展一旦按纯申报实现，那条通道就是 100% 丢失。
// 所以这道门只留给正则抽取，申报通道单列（见 extractReported）。
//
// 失败调用（IsError）仍然两条通道都不走：错误信息里的 JSON 不构成申报
// （它多半是 agent 拼错的参数被工具回显），而错误文本抽出来的只会是噪音。
func (g *Graph) Extract(ev harness.Event, round int) []Node {
	out := []Node{}
	if ev.Kind != harness.EventToolEnd {
		return out
	}
	if ev.IsError {
		// 工具本身失败时输出通常是错误信息，抽出来只会是噪音（前身没有这道闸，
		// 于是 `command not found: nmap` 里的路径也会变成 artifact 事实）。
		return out
	}

	fp := Fingerprint(ev.Output)
	n := len(ev.Output)
	source := toolSource(ev)

	// 正则抽取：必须有输出。空输出时跳过这一整段（不是提前 return——申报通道
	// 还要走，见函数头）。
	if ev.Output != "" {
		out = append(out, g.extractFromOutput(ev, round, fp, n, source)...)
	}

	// 5) agent 的 report_fact 申报（agent-asserted 层）。它与宿主抽取是**两条
	//    独立通道**：agent 申报了但工具输出里没有 ⇒ 调用方可以据此降置信度
	//    （设计文档 §一「事实从哪来」第 2 条的对账语义）。
	out = append(out, g.extractReported(ev, round, fp)...)
	return out
}

// extractFromOutput 是纯正则抽取：只依赖 ev.Output，与 report_fact 通道解耦。
// 抽出来的节点 Trust 一律 TrustHost（宿主从原始输出指纹匹配得到）。
func (g *Graph) extractFromOutput(ev harness.Event, round int, fp string, n int, source string) []Node {
	out := []Node{}
	emit := func(kind FactKind, content string, conf float64, off int) {
		c := strings.TrimSpace(content)
		if c == "" {
			return
		}
		// flag 候选永不入事实库（不变量 2）。命中即丢——它该由 gate 的候选账本
		// 接管，宿主在这里静默丢弃而不是报错，因为「输出里有 flag」是**正常**的
		// 事（`curl http://t/flag` 就该有），不是调用方的错误。
		//
		// artifact 额外过一遍裸串判定：一条**整串就是裸值**的 artifact 多半是
		// agent 自己写进文件再读回来的内容（`echo 'flag{x}' > /tmp/f; cat /tmp/f`
		// 的输出就是一个裸串）。带路径形态的内容（有斜杠/扩展名/空白）不受影响。
		if kind == FactArtifact && g.bareAnswerShaped(c, kind) {
			return
		}
		if g.answerShaped(c, kind) {
			return
		}
		out = append(out, Node{
			Kind:        NodeFact,
			FactKind:    kind,
			Content:     c,
			Raw:         snippet(ev.Output, off),
			Source:      source,
			Confidence:  conf,
			Trust:       TrustHost,
			ToolCallID:  ev.ToolCallID,
			Fingerprint: fp,
			Offset:      off,
			OutputLen:   n,
			Round:       round,
		})
	}

	// 1) 地址:端口。前身这里切 [:2000]，本实现扫全文。
	//    上限按**每类**计，而不是全局——端口多不代表凭证多，全局上限会让
	//    一份大输出把后面所有类别挤掉（前身 actionable_assets 只取前 5 条的同款错误）。
	for _, m := range firstN(ipPortRe.FindAllStringSubmatchIndex(ev.Output, -1), maxExtractPerKind) {
		ip := ev.Output[m[2]:m[3]]
		if !validIPv4(ip) {
			continue
		}
		content := ip
		if m[4] >= 0 {
			port := ev.Output[m[4]:m[5]]
			if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
				continue
			}
			content = ip + ":" + port
		}
		emit(FactTarget, content, 0.8, m[0])
	}

	// 2) 服务名 + 版本。合成一条 service 事实（`nginx/1.18.0`），因为分开记
	//    会让「哪个版本属于哪个服务」在渲染时丢失（前身分开记，注入时只能给出
	//    一堆互不相关的名字）。
	seenSvc := 0
	for _, m := range versionRe.FindAllStringSubmatchIndex(ev.Output, -1) {
		if seenSvc >= maxExtractPerKind {
			break
		}
		name := strings.ToLower(ev.Output[m[2]:m[3]])
		ver := ev.Output[m[4]:m[5]]
		if !serviceRe.MatchString(name) && !looksLikeServiceName(name) {
			continue
		}
		seenSvc++
		emit(FactService, name+"/"+ver, 0.7, m[0])
	}
	for _, m := range firstN(serviceRe.FindAllStringIndex(ev.Output, -1), maxExtractPerKind) {
		emit(FactService, strings.ToLower(ev.Output[m[0]:m[1]]), 0.6, m[0])
	}

	// 3) 凭证（B14 质量闸）。
	// 取 group 1（不含前导的非字母数字字符）——Go 的 RE2 不支持 `(?<!...)` 后顾，
	// 所以「键前不是字母数字」这个条件改用「消耗一个字符再捕获」表达；把那个字符
	// 一起当内容会让换行后的 `admin:Admin@123` 与行内的 `admin:Admin@123`
	// 变成两条事实（去重失效 ⇒ 同一凭证入图多次）。
	for _, m := range firstN(credRe.FindAllStringSubmatchIndex(ev.Output, -1), maxExtractPerKind*4) {
		raw := ev.Output[m[2]:m[3]]
		if !CredQualityOK(raw) {
			continue // 占位符 / 爆破 payload / 状态码 / 纯字母短词
		}
		emit(FactCredential, normalizeCred(raw), 0.6, m[0])
	}

	// 4) 文件路径与 URL 路径（artifact）。
	for _, m := range firstN(artifactRe.FindAllStringSubmatchIndex(ev.Output, -1), maxExtractPerKind) {
		emit(FactArtifact, ev.Output[m[2]:m[3]], 0.5, m[0])
	}
	for _, m := range firstN(urlPathRe.FindAllStringSubmatchIndex(ev.Output, -1), maxExtractPerKind) {
		emit(FactArtifact, ev.Output[m[2]:m[3]], 0.5, m[0])
	}
	return out
}

// ExtractReport 抽取 report_fact 载荷（导出，供只需要申报通道的调用方使用）。
//
// 与 Extract 一致的两条前置条件：只处理成功的 tool_end（失败调用的 Output 是
// 错误信息，里面的 JSON 不构成申报），以及**不要求 Output 非空**——载荷在
// `Details` 里，纯申报调用（没有 stdout）恰恰是它最常见的形态，用 Output 非空
// 当门会把它整条丢掉。
func (g *Graph) ExtractReport(ev harness.Event, round int) []Node {
	if ev.Kind != harness.EventToolEnd || ev.IsError {
		return nil
	}
	return g.extractReported(ev, round, Fingerprint(ev.Output))
}

// reportedFact 是 report_fact 的单条载荷（与 extensions/report_fact.ts 的
// parameters 一一对应）。
type reportedFact struct {
	Kind       string  `json:"kind"`
	Content    string  `json:"content"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

type reportPayload struct {
	Facts []reportedFact `json:"facts"`
	// Next 是 agent 建议的下一步。它由 Scheduler.Ingest 转成一条挂在本轮产出事实
	// 上的**候选**意图（enables 边）——候选不等于调度，仍要过前沿优先级。
	Next string `json:"next"`
}

// extractReported 处理 agent 申报的事实。
//
// 三条信任规则在这里执行（设计文档 §一 的三层信任表）：
//  1. Confidence 封顶 0.7 —— agent 可以很确信，但宿主不允许它把确信度当证据。
//  2. `vuln` / `foothold` **必须**带 evidence，否则**降级为低置信 artifact**
//     （不是丢弃：agent 的观察仍然值钱，只是不能自称已确认的漏洞）。
//  3. 申报的 kind 必须落在 FactKind 的合法集合里 —— `flag` 不在其中，所以
//     agent 不可能通过申报通道把 flag 塞进事实库。
//
// 注意 `negative` 的申报被**故意忽略**：negative 事实的入库条件是「带被证伪的
// 意图 id」，而 agent 不知道意图 id（那是宿主的概念）。agent 说的「这条路走不通」
// 由宿主在 Settle 时判定归属（当前轮次的活动意图），走 AddNegative。
func (g *Graph) extractReported(ev harness.Event, round int, fp string) []Node {
	payload, ok := ReportPayload(ev)
	if !ok {
		return nil
	}
	var out []Node
	for _, rf := range payload.Facts {
		kind := FactKind(strings.ToLower(strings.TrimSpace(rf.Kind)))
		content := strings.TrimSpace(rf.Content)
		if content == "" {
			continue
		}
		if !kind.Valid() || kind == FactNegative {
			// 未知类别（含 `flag`）与 negative 都不走这条通道。
			continue
		}
		if g.answerShaped(content, kind) || g.bareAnswerShaped(content, kind) {
			continue // 同上：答案不进事实库
		}
		conf := rf.Confidence
		if conf <= 0 || conf > 1 {
			conf = 0.5
		}
		if conf > AgentConfidenceCap {
			conf = AgentConfidenceCap
		}
		evidence := strings.TrimSpace(rf.Evidence)
		// 判据用 **evidence 字段**（agent 自己给的证据），而不是 ToolCallID：
		// ToolCallID 是「这次 report_fact 调用本身」，它对**所有**申报都非空，
		// 拿它当证据等于这道闸从不生效。agent 说「/upload 无类型校验」时必须
		// 指明它是**在哪次工具调用/哪段输出里**看到的。
		if (kind == FactVuln || kind == FactFoothold) && evidence == "" {
			// 降级而不是拒收：agent 说「/upload 无类型校验」但没给证据时，这仍然
			// 是一条有用的**线索**，只是它不能以「已确认的漏洞」的身份入图——
			// 否则漏洞清单会被 agent 的想象填满，而渲染进 prompt 的「已知事实」
			// 是最强的暗示（agent 会照着它去利用一个不存在的漏洞）。
			kind = FactArtifact
			conf = 0.3
		}
		out = append(out, Node{
			Kind:        NodeFact,
			FactKind:    kind,
			Content:     content,
			Raw:         truncate(content, maxRawSnippet),
			Source:      "report_fact",
			Confidence:  conf,
			Trust:       TrustAgent,
			ToolCallID:  ev.ToolCallID,
			Evidence:    evidence,
			Fingerprint: fp,
			OutputLen:   len(ev.Output),
			Round:       round,
		})
	}
	return out
}

// ReportPayload 从事件里取出 report_fact 的结构化载荷。
//
// M0 实测：`details` 原样到达、262 KB 不截断，所以主通道就是它。但**保留一条
// 从 Output 里回捞的兜底**——若某次 details 因体积被上游裁掉（设计文档 §一
// 「事实从哪来」第 3 条的降级），report_fact 的 content 文本里仍有 JSON。
func ReportPayload(ev harness.Event) (reportPayload, bool) {
	var p reportPayload
	if ev.Details != nil {
		if raw, ok := ev.Details["report_fact"]; ok {
			if b, err := json.Marshal(raw); err == nil && json.Unmarshal(b, &p) == nil {
				return p, true
			}
		}
	}
	return p, false
}

// ── B14 凭证质量闸（逐条移植 _cred_value_ok / _cred_quality_ok）──

// CredQualityOK 判定一条凭证事实能否入图（前身 `_cred_quality_ok`）。
//
// 要求：整条内容全串匹配「键=值」形态，且值通过 CredValueOK。
// 为什么要求**全串**匹配而不是前缀匹配：前身旧数据里
// `passwd: HTTPConnectionPool(host='10.x.x.x',` 会因前缀匹配取到
// `HTTPConnectionPool` 而通过初判——这条垃圾事实在注入时占据了宝贵的 top-5 名额。
func CredQualityOK(content string) bool {
	m := credFullRe.FindStringSubmatch(strings.TrimSpace(content))
	if m == nil {
		return false
	}
	quoted := m[1] != "" || m[2] != ""
	val := ""
	for _, gg := range m[1:] {
		if gg != "" {
			val = gg
			break
		}
	}
	return CredValueOK(val, quoted)
}

// CredValueOK 判定一个值是否像真凭证（前身 `_cred_value_ok`）。
//
// 实测漏网的噪音值：`css` / `final` / `enable_queue` / `HTTPConnectionPool` /
// `zzzzz`。规则：
//   - 占位符与爆破 payload 丢弃；
//   - 单字符种类 ≤ 2（`zzzzz` / `11111`）丢弃 —— 「重复」是填充的指纹；
//   - 至少含一个字母（状态码 / 端口号丢弃）；
//   - **未加引号时**还要求含数字或符号，或长度 ≥ 12。
//
// 最后一条是本闸最关键的一条：纯字母短词在本项目的数据里**全是页面文本**
// （css / final / enable），而真凭证 `admin:Admin@123`、`admin:hunter2xyz`、
// `john.doe` 都含数字或符号。加引号时放宽——引号本身就是「这是一个人为的值」
// 的信号（表单值 / 配置项）。
func CredValueOK(val string, quoted bool) bool {
	v := strings.TrimSpace(val)
	if v == "" || credPlaceholders[strings.ToLower(v)] {
		return false
	}
	if len(uniqueRunes(v)) <= 2 {
		return false
	}
	if !strings.ContainsFunc(v, isASCIILetter) {
		return false
	}
	if quoted {
		return true
	}
	// 下划线是**标识符字符**，不是「像密钥」的信号。这条与 answer 包
	// `looksLikeRawValue` 的处理一致（那里写着：否则 `enable_queue` 这种纯小写
	// 下划线串会被当成答案），而前身 `_cred_value_ok` 用 `not ch.isalpha()` 判
	// 「含符号」，把下划线也算成了符号——于是 B14 语料里的 `enable_queue` 从这道
	// 闸漏了过去。这是移植时必须修掉的一处：语料是判据的验收标准，不是参考。
	hasSymbol := strings.ContainsFunc(v, func(r rune) bool { return !isASCIILetter(r) && r != '_' })
	if hasSymbol {
		return true
	}
	// 纯字母（含下划线）的串：只有**大小写混合且够长**才认（camelCase 风格的密钥）。
	// 单靠长度会放进 `enable_queue`、`abcdefghijkl` 这类标识符/单词。
	hasUpper, hasLower := false, false
	for _, r := range v {
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
	}
	return hasUpper && hasLower && len(v) >= 12
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func uniqueRunes(s string) map[rune]bool {
	m := map[rune]bool{}
	for _, r := range s {
		m[r] = true
	}
	return m
}

// normalizeCred 把一条凭证规范化成 `键:值`（去空白、小写键）。
//
// 为什么规范化：`Admin : Admin@123` 与 `admin:Admin@123` 是同一个凭证，不归一
// 会让去重失效（同一凭证入图多次，正是前身「重复读取看起来像新事实」的机制）。
// 注意**值不做小写化**——密码是大小写敏感的，小写化会把真凭证改成错的。
//
// 值里的引号只在**成对出现**时才剥掉：`password: 'a'b` 这种输出里引号是内容
// 的一部分，无条件 Trim 会改掉真凭证（而去重键用的是 normalize 后的小写，
// 这里多剥一个字符就可能让两条不同凭证并成一条）。
func normalizeCred(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, ":=")
	if i < 0 {
		return normalize(s)
	}
	k := strings.ToLower(strings.TrimSpace(s[:i]))
	v := strings.TrimSpace(s[i+1:])
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return k + ":" + v
}

// ── 指纹与小工具 ──

// Fingerprint 返回输出的 sha256 前 12 个十六进制字符。
//
// 前身把输出截断到 2000 字符再抽，本实现改为「抽取用全文、落盘只存指纹」。
// 指纹的用途有两个：一是证明「这条事实确实来自某份完整输出」，二是让两次
// 抽取的结果可以按指纹比对（同一份输出重放 ⇒ 同一批事实，不产生新节点）。
func Fingerprint(output string) string {
	sum := sha256.Sum256([]byte(output))
	return hex.EncodeToString(sum[:])[:12]
}

// toolSource 生成人可读的来源标记（`bash: nmap -sV 10.0.0.1`）。
//
// 来源是渲染进 prompt 的关键字段：agent 看到 `← bash: curl .../login` 才知道
// 这条事实是怎么来的、能不能信。前身 blackboard 的 source 字段就是干这个的，
// 但它的 source 只截了命令前 100 字符——命令末尾才是参数，截掉等于没记。
func toolSource(ev harness.Event) string {
	tool := ev.Tool
	if tool == "" {
		tool = "tool"
	}
	var arg string
	if ev.Args != nil {
		for _, k := range []string{"command", "cmd", "file_path", "path", "pattern", "url"} {
			if v, ok := ev.Args[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					arg = s
					break
				}
			}
		}
	}
	arg = strings.Join(strings.Fields(arg), " ")
	if arg == "" {
		return tool
	}
	return tool + ": " + truncate(arg, 200)
}

// snippet 取命中位置附近的取证片段。
//
// 用**字节**偏移定位（Offset 记的也是字节偏移）。工具输出通常是 ASCII；若命中
// 落在多字节字符中间，取片段时会向前后扩一点以落在字符边界上，避免产生非法
// UTF-8（渲染进 prompt 时会显示成乱码，前身没处理过这个）。
func snippet(s string, off int) string {
	if off < 0 {
		off = 0
	}
	if off >= len(s) {
		off = max(0, len(s)-1)
	}
	start := off - maxRawSnippet/3
	if start < 0 {
		start = 0
	}
	for start > 0 && start < len(s) && !utf8Start(s[start]) {
		start--
	}
	end := start + maxRawSnippet
	if end > len(s) {
		end = len(s)
	}
	for end > start && end < len(s) && !utf8Start(s[end]) {
		end--
	}
	return strings.Join(strings.Fields(s[start:end]), " ")
}

// utf8Start 报告 b 是否是一个 UTF-8 字符的首字节。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// firstN 取切片前 n 个元素（n <= 0 时返回全部）。
func firstN[T any](xs []T, n int) []T {
	if n <= 0 || len(xs) <= n {
		return xs
	}
	return xs[:n]
}

// looksLikeServiceName 判断一个词是否像服务/软件名（用于版本号抽取）。
//
// 为什么不直接要求 serviceRe 命中：`OpenSSH 8.2p1`、`OpenResty 1.21`、
// `gunicorn 20.0` 这些真实指纹不在固定服务名表里，只认表会漏掉整类事实。
// 判据放宽到「短标识符 + 后面跟版本号」——版本号形态本身就是强信号，
// 而随机文本里 `foo 1.2.3` 这种组合极其罕见。
func looksLikeServiceName(s string) bool {
	if len(s) < 3 || len(s) > 25 {
		return false
	}
	return !strings.ContainsAny(s, " \t")
}

// validIPv4 校验八位组范围。前身的正则只数位数，会把 `999.1.2.3` 和
// `1.2.3.4.5`（后半段被 `\b` 断开）当地址。
func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	// 排除版本号串：四段全是个位数且都 < 10 的，多半是 `1.2.3.4` 形式的版本号，
	// 但真正的内网地址也可能长这样（10.0.0.1）。所以只排除**首段为 0** 的
	// （`0.0.0.0` 之类）以及明显不可能的私有段之外的值——判据保守，宁可多留。
	if parts[0] == "0" {
		return false
	}
	return true
}

// EnvelopeHit 报告内容里是否出现本题的信封形态（导出，供 gate 等包复用同一判据）。
//
// 与 answer.Shape.Contains 的关系：Contains 会连**裸串**一起挖（它服务于「从一段
// 长文本里找候选」），而这里只判信封。两处都需要，但语义不同，所以不合并——
// 合并会让其中一处的误判率上升（前身 B55 的教训是「同一判定散落四处会漂移」，
// 但那条教训说的是**同一判定**；这里是两个不同的判定，强行合并才是问题）。
func (g *Graph) EnvelopeHit(content string) bool { return envelopeHit(g.Shape, content) }

// AnswerShaped 导出「内容是否就是答案」的判据，供调用方在入图前预筛
// （避免把注定被拒的内容反复送进 AddFact 再处理 error）。
func (g *Graph) AnswerShaped(content string, kind FactKind) bool {
	return g.answerShaped(content, kind)
}

// ShapeOf 返回本题的答案形态（只读用途）。
func (g *Graph) ShapeOf() answer.Shape { return g.Shape }

// Describe 返回一条事实的一行人可读描述（渲染与报告共用，保证两处措辞一致）。
func Describe(n *Node) string {
	if n == nil {
		return ""
	}
	if n.IsIntent() {
		return fmt.Sprintf("[%s] %s", n.IntentKind, n.Goal)
	}
	return fmt.Sprintf("[%s] %s", n.FactKind, n.Content)
}
