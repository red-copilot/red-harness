// Package gate 是候选答案的账本与来源闸。
//
// 它要修掉的具体事故有三个（都来自前身）：
//
//  1. **提交时机**：旧契约把提交放进逐事件回调，于是每次 tool_execution_end
//     都重遍历全部候选重复提交。本包把「记账」与「提交」彻底分开：Observe
//     只记账、绝不 IO；轮末由根包 harvest 调 New() 取未提交的候选。
//
//  2. **洗白路径**：agent 写 `echo 'flag{我编的}' > /tmp/f` 再 `cat /tmp/f`，
//     输出里就出现了 flag —— 自己的猜测被自己读回来，看起来像「观测」。
//     本包的对策是**首现优先**：一个答案串的族别由它**首次出现的位置**决定，
//     之后怎么读回来都改不了族别。前身用 3,592 行 shell/AST 解析追这条路径，
//     这里用一条规则覆盖它。
//
//  3. **判错账本的明文泄漏**：flag 原文被写进会回灌下一场的文件。本包的
//     指纹只含 sha256[:8] + 长度 + 首尾字符，绝不含明文。
package gate

import (
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// Provenance 沿用根包定义（harness.Provenance），这里只做别名，免得两处
// 口径漂移 —— 前身 B55 事故就是「同一个判定在四个调用点各写一遍」。
type Provenance = harness.Provenance

const (
	ProvenanceObserved   = harness.ProvenanceObserved
	ProvenanceDerived    = harness.ProvenanceDerived
	ProvenanceFabricated = harness.ProvenanceFabricated
)

// RejectReason 是候选被拒的原因。前身 verify.py 有 8 种 reject_reason，
// 这里只保留**决定族别的那几种**（事后从字符串重建的复杂度不值得移植）。
const (
	// ReasonCommandAuthored：产生输出的命令参数里就含这个答案 ⇒ 首现不是观测。
	// 对应前身的 agent_authored。
	ReasonCommandAuthored = "agent_authored"
	// ReasonSelfReadback：命令在读 agent 自己写的文件（FLAG/MEMORY/黑板/
	// todolist/转录，或重定向目标）⇒ 读回自写内容，不是系统产出。
	// 对应前身的 agent_authored 的第二类。
	ReasonSelfReadback = "self_readback"
	// ReasonNotGrounded：答案只出现在 agent 的散文里，从未见于任何工具输出。
	// 对应前身的 not_grounded。
	ReasonNotGrounded = "not_grounded"
	// ReasonFormatRejected：连答案形态都不像（占位符 / 低熵 / 表单字段名 /
	// 状态码 / SQLi payload 残片）。对应前身的 placeholder_pattern / low_entropy。
	ReasonFormatRejected = "format_rejected"
)

// ── 首现优先判定的两个辅助判据 ──

// shapeMarkRe 抓「形状标记」：`flag{` 这种前缀本身。
//
// 为什么需要它：`answer.Shape.Match` 要求信封**闭合**，而
// `echo 'flag{x}' > /tmp/f` 这条命令的整串文本里，`flag{x}` 被 Match 认出
// 的同时，Match 也会把重定向目标 `/tmp/f` 当成裸串候选（形状真源是
// answer 包，它按「值像不像答案」判定，`/tmp/f` 恰好像）。我们只要判断
// 「命令里有没有答案形状」，不需要它的结果 —— 用标记正则即可，既便宜又
// 不会因为 Match 的噪音产生假阴性。
//
// 关键性质：它对 `curl http://t/flag` 返回 false（有 `flag` 词但没有 `{`），
// 这正是四情形矩阵里「观测」那一行的成立条件。
var shapeMarkRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]{1,15}\{`)

// commandCarriesShape 报告一条命令的文本里是否含答案形状。
//
// 这就是「echo flag{x} ⇒ 非观测」与「python3 solve.py ⇒ 观测」之间**唯一**
// 的区别。判据必须是语法形状而不是粗糙的子串包含 —— 后者会把
// `curl http://t/flag` 也判成「含答案形状」，直接掐掉真观测。
//
// 具体到实现：**不能**在这里用 `sh.Contains(cmd)` 兜底。answer 包的裸串识别
// 口径是「宁可多认不可漏认」，`curl -s http://10.0.0.1/login` 里的
// `10.0.0.1/login` 会被它当成候选 —— 于是每一条带 IP 的命令都会被判成
// 「含答案形状」，四情形矩阵里「观测」的两行同时挂掉（这不是假设，第一版
// 就是这么写的，测试直接抓到）。信封形态一律以 `{` 结尾（answer.Infer /
// answer.New 都只产生这种形态），所以标记正则足够覆盖；命令里出现**裸串**
// 答案的情形由 materializedIn（逐字/编码匹配具体候选）负责。
//
// 后人若要「简化」回 Contains，先看 answer/contains_test.go：那里钉住了
// `Shape.Contains("nmap -sV 10.0.0.1") == true`（裸串形态下 Match 会挖出
// `10.0.0.1` 当候选）。**Contains 不能用作拒收判据** —— 在 gate 这一侧，
// 用了它就会把每一条打靶命令判成 agent 自造，真观测全被掐掉；在 dag 那一侧，
// 用了它会把 IP/路径这类正常事实当 flag 拒收，图会永久学不到任何东西。
func commandCarriesShape(cmd string, sh answer.Shape) bool {
	if cmd == "" {
		return false
	}
	return shapeMarkRe.MatchString(cmd)
}

// stateFileRe 匹配「agent 自己的状态文件」名。前身 _AGENT_STATE_BASE_RX 的
// 移植：`cat FLAG`、`cat /tmp/work/MEMORY.md`、`cat _blackboard.json` 都命中。
//
// 为什么必须区分大小写（`FLAG` 大写）而 `flag.txt` 小写也认：前身的状态文件
// 实际叫 FLAG / MEMORY.md，而靶标端点的路径常常是 `/flag`（小写）。若做成
// 大小写不敏感，`curl http://t/flag` 会被误判成「读自己的文件」，
// 四情形矩阵的第二行就挂了 —— 那是一条**真观测**被掐掉。
var stateFileRe = regexp.MustCompile(`(?:^|[/\\"' ])(?:` +
	`FLAG(?:\.(?:txt|md|json|log))?|SOURCE(?:\.(?:txt|md|json|log))?|` +
	`MEMORY(?:\.md)?|MEMORY\.md|_?blackboard[\w.\-]*|todolist[\w.\-]*|` +
	`tried_commands[\w.\-]*|_transcripts|notes[\w.\-]*` +
	`)(?:[\s"'` + "`" + `,;)|&<>]|$)`)

// stateFileReCI 是 stateFileRe 的**收窄后**大小写不敏感变体。
//
// ⚠️ 这里**只保留 `flag(?:\.(?:txt|md|json|log))` 一项**，其余名字回到
// 大小写敏感（由 stateFileRe 负责）。原因是一类真实的损失：
//
//	原实现让裸小写 `notes` / `memory` / `todolist` / `_blackboard*` /
//	`tried_commands*` / `_transcripts` 也 CI 匹配，于是靶标端点上恰好叫这些
//	名字的路径被误判 —— `curl -s http://t/flag.txt`、
//	`curl -s http://10.0.0.1/notes`、`grep -r memory /etc` 全被判成
//	self_readback ⇒ locked=true ⇒ **该候选永久不可提交**。
//
// 这正是本文件记录的「29 条被拒里 19 条实为正确答案」那一类：误判的代价是
// 把真观测掐掉，而掐掉之后**没有任何报错**。
//
// 为什么 `flag.txt` 这一项要留在 CI 里：前身的状态文件确实有小写的
// `flag.txt` / `flag.md`，而靶标端点上的 `/flag.txt` 是另一回事 —— 两者在
// **词法**上不可区分，靠的是 URL 已在调用方摘除。这是刻意的取舍：宁可在这
// 一项上保守（认它），也不要在 notes/memory 这类常见靶标路径上误判。
var stateFileReCI = regexp.MustCompile(`(?i)(?:^|[/\\"' ])(?:` +
	`flag(?:\.(?:txt|md|json|log))` +
	`)(?:[\s"'` + "`" + `,;)|&<>]|$)`)

// readsOwnState 报告这条命令是否在读写 agent 自己的状态文件。
//
// `cat FLAG`（读自己的投递文件）必须判非观测：前身 B37 的教训是「agent 把
// 结论写进这些文件后再 cat，与 echo 自造同源」。这里做的是**词法**判定而不是
// 前身那套 shell 分词 —— 因为首现优先已经覆盖了绝大多数洗白路径，这里只需
// 挡住最直白的一类（文件名精确出现），多写的每一行解析器都会变成新的误判面。
//
// **先摘 URL，再判文件名。** 这一步是必须的，且和 materializedInCommand 里
// 摘 URL 是同一个理由：靶标端点的路径与 agent 的状态文件名**同形**。
// `curl -s http://t/flag.txt` 里的 `/flag.txt` 是靶标路径，不摘的话会被判成
// self_readback ⇒ locked=true ⇒ 该候选永久不可提交。原实现只靠「CI 变体里
// 不放裸 flag」躲过了 `/flag`，但躲不过 `/flag.txt`、`/notes`、`/memory`。
func readsOwnState(cmd string) bool {
	if cmd == "" {
		return false
	}
	cmd = stripURLs(cmd)
	if stateFileRe.MatchString(cmd) || stateFileReCI.MatchString(cmd) {
		return true
	}
	// pi 自己的会话记录与 HOME：装着完整对话（含 harness prompt 原文）。
	if piStateRe.MatchString(cmd) {
		return true
	}
	// 裸重定向到状态文件：`> FLAG` / `>> MEMORY.md` / `2>flag.txt`
	if redirTargetRe.MatchString(cmd) {
		return true
	}
	return false
}

// redirTargetRe 抓「重定向目标就是状态文件」。
var redirTargetRe = regexp.MustCompile(`(?:^|[^>])>{1,2}\s*["']?` +
	`(?:FLAG(?:\.(?:txt|md|json|log))?|MEMORY(?:\.md)?|_?blackboard[\w.\-]*|` +
	`todolist[\w.\-]*|tried_commands[\w.\-]*)`)

// piStateRe 抓 **pi 自己的会话记录目录与 HOME**。
//
// 为什么单独一条：这两个目录里存的是**完整对话原文**——包含 harness 注入的
// prompt（以及 prompt 里的格式示例 `flag{...}`）。读它们等于读自己的输入，
// 输出里的任何「答案形状」都是自己写的，不是靶标产出的。
//
// 实测事故（授权 TSecBench 真跑）：agent 执行 `cat /work/.pi-sessions/*.jsonl`
// 做侦察，输出里带着 prompt 中的字面量 `flag{...}`，gate 判成观测族并提交，
// 平台以 501 拒绝——整轮解题动作被一条自读路径带偏。
//
// 尾部的分隔符类含 `*` 与 `/`：命令里常见 `cat /work/.pi-sessions/*.jsonl`
// 与 `ls -la /work/.pi-sessions` 两种形态，glob 的 `*` 后面直接跟文件名。
var piStateRe = regexp.MustCompile(`(?:^|[/\\"' ])\.pi-(?:sessions|home)(?:[/\\*"' ]|$)`)

// heredocRe 抓 heredoc 头。脚本体（`cat > solve.py <<'EOF' … EOF`）会被摘掉
// 后再判定，否则脚本注释里出现的候选值会污染「命令是否含答案形状」。
var heredocRe = regexp.MustCompile(`<<\s*-?\s*['"]?(?P<tag>[A-Za-z_][A-Za-z0-9_]*)['"]?`)

// stripHeredocBodies 把 heredoc 载荷清空（保留头部）。
//
// 前身 B40 的理由：脚本体是 agent 的**代码**，不是命令参数。`python3 solve.py`
// 必须判观测，即使 solve.py 里写了 `print(flag)`；同理 heredoc 里的脚本正文
// 不该让 `python3 - <<EOF` 变成「命令含答案形状」。
func stripHeredocBodies(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	b := []byte(cmd)
	for _, m := range heredocRe.FindAllStringSubmatchIndex(cmd, -1) {
		tag := cmd[m[2]:m[3]]
		nl := strings.IndexByte(cmd[m[1]:], '\n')
		if nl < 0 {
			continue
		}
		start := m[1] + nl + 1
		rest := cmd[start:]
		end := len(cmd)
		for _, line := range strings.Split(rest, "\n") {
			if strings.TrimSpace(line) == tag {
				end = start + strings.Index(rest, line)
				break
			}
			start += len(line) + 1
		}
		for i := m[1]; i < end && i < len(b); i++ {
			if b[i] != '\n' && b[i] != '\r' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// quotedPayloadRe 抓单引号/双引号包起来的载荷（含转义）。
var quotedPayloadRe = regexp.MustCompile(`'[^'\n]*'|"(?:[^"\\\n]|\\.)*"`)

// quotedPayloads 返回命令里所有带引号载荷的**内容**（不含引号本身）。
//
// 为什么需要内容视图：裸串形态的答案常常**就是**引号里的那个词
// （`echo 'hunter2xyz' > /tmp/f`）。只摘引号不看内容，这条路径就完全看不见
// —— 它没有 `{` 形状标记，靠形状正则永远抓不到。
func quotedPayloads(cmd string) []string {
	var out []string
	for _, m := range quotedPayloadRe.FindAllString(cmd, -1) {
		if len(m) < 2 {
			continue
		}
		out = append(out, m[1:len(m)-1])
	}
	return out
}

// urlTokenRe 抓 URL 形态的 token（含 scheme）。
var urlTokenRe = regexp.MustCompile(`\S*://\S*`)

// stripURLs 摘掉命令里的 URL。
//
// 为什么必须摘：裸串候选与 URL 片段**同形** —— `curl http://10.0.0.1/flag`
// 的输出里，形状真源会给出 `//10.0.0.1/flag` 这样的候选，而它逐字出现在
// 命令里。若不摘 URL，「命令里出现了候选」这条判据会把每一条打靶命令都判成
// 自造，四情形矩阵里「观测」的两行同时挂掉。
func stripURLs(cmd string) string {
	if !strings.Contains(cmd, "://") {
		return cmd
	}
	return urlTokenRe.ReplaceAllString(cmd, " ")
}

// materializedInCommand 报告「候选（或它的编码变体）被 agent 写进了命令」。
//
// 两档，都是**逐字**匹配（不是子串包含的粗糙判定）：
//
//  1. 引号载荷的内容 —— `echo 'hunter2xyz' > /tmp/f`、`./validate 'flag{x}'`、
//     `python3 -c "…b64decode('ZmxhZ3t4fQ==')"`。裸串候选遇到带 scheme 的
//     载荷（URL）时跳过：URL 与裸串候选同形，见 stripURLs 的注释。
//  2. 命令骨架（已摘引号与 URL）—— 覆盖不加引号的物化。
func materializedInCommand(cmd, cand string) bool {
	if cmd == "" || cand == "" {
		return false
	}
	_, isEnvelope := envelopeBody(cand)
	for _, p := range quotedPayloads(cmd) {
		if !isEnvelope && strings.Contains(p, "://") {
			continue
		}
		if materializedIn(p, cand) {
			return true
		}
	}
	return materializedIn(stripURLs(stripQuotedPayloads(cmd)), cand)
}

// quotedCommandCandidates 从命令的**引号载荷内容**里抽候选（只取裸串形态）。
//
// 信封形态由 shapeCandidates 从命令整串文本里覆盖，这里只补裸串那一条：
// `echo 'hunter2xyz' > /tmp/f` 没有任何形状标记，唯一的线索就是引号里的内容。
// 带 scheme 的载荷（URL）整段跳过 —— URL 与裸串候选同形，见 stripURLs。
func quotedCommandCandidates(sh answer.Shape, cmd string) []string {
	if cmd == "" {
		return nil
	}
	var out []string
	for _, payload := range quotedPayloads(cmd) {
		if strings.Contains(payload, "://") {
			continue
		}
		for _, cand := range outputCandidates(sh, payload) {
			if _, isEnvelope := envelopeBody(cand); isEnvelope {
				continue
			}
			// 地址/路径形态的值**不是** agent 自造的答案：`nmap -sV '10.0.0.1'`、
			// `cat '/etc/passwd'` 里的引号载荷恰好通过形状真源的裸串判定（它的
			// 口径是「宁可多认」，漏认的代价是丢分），但把它们记成候选就是前身
			// B14 那条「61 条垃圾事实挤掉真信号」的老路 —— 而幻觉族的计数是要
			// 参与阈值判断的。这里只做一次形态过滤，不改变形状真源。
			//
			// 【本实现的收紧，不在前身清单里】前身 _AGENT_STATE_BASE_RX 那套只
			// 管状态文件名，没有「命令引号载荷里的地址/路径」这一条；这条是本
			// 实现为了让裸串形态不污染幻觉族计数而加的，将来对照前身数据时要
			// 记得它没有对应项。
			if addrLikeRe.MatchString(cand) {
				continue
			}
			out = append(out, cand)
		}
	}
	return out
}

// addrLikeRe 抓「地址/路径」形态：IPv4、Windows 盘符路径、绝对路径。
var addrLikeRe = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$|^[A-Za-z]:[\\/]|^/`)

// stripQuotedPayloads 摘掉命令里所有带引号的载荷，只留命令骨架。
//
// 前身 B40 的教训：agent 常把候选写进 helper 的引号参数里
// （`./validate 'flag{x}'`）。引号内容是「被物化的候选」，不是命令本身。
// 注意 URL 也可能在引号里（`curl 'http://t/flag'`）—— 所以只有在需要时
// 才用这个视图（见 firstOccurrence 的注释）。
func stripQuotedPayloads(cmd string) string {
	return quotedPayloadRe.ReplaceAllString(cmd, " ")
}

// materializations 返回候选的常见字面编码变体（原文、裸 body、hex、base64、
// urlsafe-base64）。前身 _candidate_materializations 的移植。
//
// 为什么需要：`python3 -c "print(__import__('base64').b64decode('ZmxhZ3t4fQ=='))"`
// 里没有 `flag{` 这个字面标记，但它把答案物化进了命令。只按「形状标记」判定
// 会漏掉这条路径 —— 这是前身专门为它写过一段代码的原因。
//
// 短于 8 的变体一律丢弃：短串在无关代码里到处都是，会带来大量假阳性。
func materializations(cand string) []string {
	raw := strings.TrimSpace(cand)
	if raw == "" {
		return nil
	}
	values := []string{raw}
	if body, ok := envelopeBody(raw); ok && body != "" {
		values = append(values, body)
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, variant := range []string{
			v,
			hex.EncodeToString([]byte(v)),
			base64Std(v),
			strings.TrimRight(base64URL(v), "="),
		} {
			if len(variant) < 8 || seen[variant] {
				continue
			}
			seen[variant] = true
			out = append(out, variant)
		}
	}
	return out
}

// materializedIn 报告候选（或它的常见编码）是否逐字出现在文本里。
//
// 大小写口径与前身一致：**原文与裸 body** 大小写无关（flag 的匹配一贯如此），
// 但 hex/base64 变体**保持大小写**——base64 是大小写敏感的编码，把它折成小写
// 去比会引入大量假阳性（`ZmxhZ3t4fQ==` 折成小写后与无关文本撞车的概率不低）。
func materializedIn(text, cand string) bool {
	if text == "" || cand == "" {
		return false
	}
	folded := strings.ToLower(text)
	raw := strings.TrimSpace(cand)
	plain := map[string]bool{raw: true}
	if body, ok := envelopeBody(raw); ok && body != "" {
		plain[body] = true
	}
	for _, v := range materializations(cand) {
		if plain[v] {
			if strings.Contains(text, v) || strings.Contains(folded, strings.ToLower(v)) {
				return true
			}
			continue
		}
		if strings.Contains(text, v) {
			return true
		}
	}
	return false
}

// envelopeBody 取 `xxx{body}` 里的 body。非信封形态返回 ("", false)。
func envelopeBody(s string) (string, bool) {
	open := strings.IndexByte(s, '{')
	if open <= 0 || !strings.HasSuffix(s, "}") {
		return "", false
	}
	return s[open+1 : len(s)-1], true
}

// ── base64（自实现，避免为两个调用点引入依赖面）──

const b64std = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func base64Encode(s, alphabet string) string {
	src := []byte(s)
	var b strings.Builder
	for i := 0; i < len(src); i += 3 {
		var n uint32
		rem := len(src) - i
		n = uint32(src[i]) << 16
		if rem > 1 {
			n |= uint32(src[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(src[i+2])
		}
		b.WriteByte(alphabet[(n>>18)&0x3f])
		b.WriteByte(alphabet[(n>>12)&0x3f])
		if rem > 1 {
			b.WriteByte(alphabet[(n>>6)&0x3f])
		} else {
			b.WriteByte('=')
		}
		if rem > 2 {
			b.WriteByte(alphabet[n&0x3f])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

func base64Std(s string) string { return base64Encode(s, b64std) }
func base64URL(s string) string { return base64Encode(s, b64url) }

// ── 凭证质量闸（B14 移植）──
//
// 前身 `blackboard.py` 的 `_CRED_RX` 是前缀匹配，于是
// `passwd: HTTPConnectionPool(host='10.x.x.x',` 会取到 HTTPConnectionPool 而
// 通过判定 —— 实测某题攒了 61 条 credential 事实，注入下一场时把真信号挤没。
// B14 的修法是**整条事实必须「键=值」到底**：尾部残留（括号/引号/换行）即判
// 噪音。这里逐条移植，作为 gate 的格式闸在裸串形态下的补充。

// credPlaceholders 是 B14 的 _CRED_PLACEHOLDERS（与 answer 包的占位符表同源）。
var credPlaceholders = map[string]bool{
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

// credKeyRe 抓「键 + 分隔符」开头的行。键表与 answer 包的 assignmentRe 一致，
// 因为两者判的是同一件事：这一行长得像赋值，而不是值。
var credKeyRe = regexp.MustCompile(`^[ \t]*(?i:username|user|login|admin|root|password|passwd|pwd|pass|name|` +
	`email|account|token|key|secret|host|hostname|domain|path|url|port|id)[ \t]*[:=]`)

// credFullRe 要求整行都是「键=值」形态，尾部不得有残留（B14 的 _CRED_FULL_RX）。
var credFullRe = regexp.MustCompile(`^[ \t]*(?i:user|login|admin|root|password|passwd|pwd|pass)[ \t]*[:=][ \t]*` +
	`(?:"([^"\n]{3,64})"|'([^'\n]{3,64})'|([A-Za-z0-9!@#$%^&*_\-\.]{3,64}))[ \t]*\**[ \t]*$`)

// credQualityOK 判定一行「键=值」是否够格当凭证（B14 的 _cred_quality_ok）。
func credQualityOK(line string) bool {
	m := credFullRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	quoted := m[1] != "" || m[2] != ""
	val := m[1]
	if val == "" {
		val = m[2]
	}
	if val == "" {
		val = m[3]
	}
	return credValueOK(val, quoted)
}

// credValueOK 判定「值」是否像真凭证。
//
// 规则（B14 复核）：占位符与爆破 payload 丢弃；单字符重复（zzzzz/11111）丢弃；
// 至少含一个字母（状态码/端口丢弃）；未加引号时还要求含数字或符号，或长度 ≥ 12
// —— 纯字母短词在本项目数据里全是页面文本（css/final/enable），而真凭证
// admin:Admin@123、admin:hunter2xyz、john.doe 都含数字或符号。
func credValueOK(val string, quoted bool) bool {
	v := strings.TrimSpace(val)
	if v == "" || credPlaceholders[strings.ToLower(v)] {
		return false
	}
	if len(uniqueRunes(v)) <= 2 {
		return false
	}
	if !hasAlpha(v) {
		return false
	}
	if quoted {
		return true
	}
	return hasNonAlpha(v) || len(v) >= 12
}

// lineAt 取 text 里包含 pos 的那一行（含边界裁剪）。
func lineAt(text string, pos int) string {
	if pos < 0 || pos >= len(text) {
		return ""
	}
	start := strings.LastIndexByte(text[:pos], '\n') + 1
	end := strings.IndexByte(text[pos:], '\n')
	if end < 0 {
		end = len(text)
	} else {
		end += pos
	}
	return text[start:end]
}

// credLineOK 判定「从这一行里挖出的裸串候选」是否可信。
//
// 只在候选来自**赋值形态的行**时收紧：`admin:Admin@123` 认（值是凭证），
// `passwd: HTTPConnectionPool(host='10.x.x.x',` 整行丢弃（尾部残留 ⇒ 噪音）。
// 非赋值行（`john.doe`、`curl` 输出里的一串 token）不受这条约束 —— 否则真
// 密码会被误杀。
func credLineOK(text, cand string) bool {
	idx := strings.Index(text, cand)
	if idx < 0 {
		return true
	}
	line := lineAt(text, idx)
	if !credKeyRe.MatchString(line) {
		return true
	}
	return credQualityOK(line)
}

// outputCandidates 从工具输出里挖候选：信封形态照收，裸串形态过 B14 质量闸。
//
// 为什么裸串要额外过一道闸：answer 包的 Match 在裸串形态下的口径是「宁可多认
// 不可漏认」（漏认 = 丢分），所以它会从噪音行里挖出 HTTPConnectionPool 这种
// 形似密钥的片段。gate 这一层再收紧一次，噪音就不会进入候选账本 —— 前身
// 61 条垃圾凭证挤掉真信号的教训。
func outputCandidates(sh answer.Shape, text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, cand := range sh.Match(text) {
		if _, isEnvelope := envelopeBody(cand); !isEnvelope {
			if !credLineOK(text, cand) {
				continue
			}
		}
		out = append(out, cand)
	}
	return out
}

// ── 指纹 ──

// Fingerprint 返回一个答案的指纹。
//
// 这是 `answer.Fingerprint` 的**转发**，不再自己实现。原因：指纹格式必须唯一，
// 否则跨包无法 join——gate 的判错账本用指纹做键，dag 把指纹渲染进 prompt 给
// agent 看，报告层要把两边对上。此前两处各有一份实现，格式还不同
// （gate 是 `6326baf8:24:f}`，dag 是 `fp:6326baf8/len=24/f…}`），字符串比较
// **永远不匹配**，跨包对照是坏的。
//
// 保留这个薄封装而不是让调用方直接调 answer：gate 的测试与外部使用者已经按
// `gate.Fingerprint` 写好了，转发比全量改名改动面小。
func Fingerprint(flag string) string { return answer.Fingerprint(flag) }

func uniqueRunes(s string) map[rune]bool {
	m := map[rune]bool{}
	for _, r := range s {
		m[r] = true
	}
	return m
}

func hasAlpha(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func hasNonAlpha(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
