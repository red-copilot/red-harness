// Package answer 定义「答案长什么样」——本题可接受的答案形态，以及一段文本是否
// 长得像答案。
//
// 这个包存在的理由（前身的血）：答案形状的判定散落在四处调用点，各自实现，长期
// 漂移（前身 B55 事故）。所以这里定义**唯一真源**，其余地方一律引用。
//
// 另一个职责是给 DAG 提供「答案形状内容拒入图」的判据：agent 写
// `echo 'flag{x}' > /tmp/f` 再 `cat /tmp/f`，输出里就出现了 flag——自己的猜测被
// 自己读回来，洗成了「观测」。宿主在图的不变量里调用 Shape.LooksLike 拒收这类
// 内容，洗白路径从根上不存在（详见 dag 的不变量）。
package answer

import (
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// Envelope 是一个「信封」形态：prefix 开头、suffix 结尾，中间是载荷。
// 典型：flag{...}、CTF{...}、KEY{...}。
type Envelope struct {
	Prefix string // 例如 "flag{"
	Suffix string // 例如 "}"
}

// Shape 是本题可接受的答案形态集合。零值 Shape 不接受任何形态。
type Shape struct {
	// Envelopes 按优先级排列的信封形态。
	Envelopes []Envelope
	// AllowRaw 为真时，除信封外还接受「裸串」答案（密码 / hash / 密钥）。
	// 由题面是否提到这类字样决定。
	AllowRaw bool
	// RawMinLen 是裸串的最小长度（含）。默认 6。
	RawMinLen int
	// RawMaxLen 是裸串的最大长度（含）。默认 200。
	RawMaxLen int
}

// 题面里出现这些字样 ⇒ 答案可能是裸串，不是 flag{...}。
var rawHints = []string{
	"密码", "口令", "密钥", "哈希", "hash", "password", "passwd", "pwd",
	"secret", "token", "key", "flag 值", "原始值",
}

// 这些值即使满足长度/字符集条件，也**不是**答案：它们是占位符、角色名或
// 常见爆破 payload。前身 B14 的 `_CRED_PLACEHOLDERS` 逐条移植。
var placeholderValues = map[string]bool{
	"test": true, "testing": true, "tests": true, "guest": true, "zzz": true,
	"wrongpass": true, "example": true, "changeme": true, "foo": true, "bar": true,
	"baz": true, "asdf": true, "qwerty": true, "letmein": true, "hello": true,
	"world": true, "password": true, "passwd": true, "pwd": true, "pass": true,
	"secret": true, "placeholder": true, "yourpassword": true, "your_password": true,
	"xxx": true, "abcd": true, "abcd1234": true, "none": true, "null": true,
	"undefined": true, "todo": true, "tbd": true, "string": true, "value": true,
	"demo": true, "sample": true, "flag": true, "admin": true, "root": true,
	"user": true, "login": true, "username": true, "true": true, "false": true,
	"enabled": true, "disabled": true, "default": true, "public": true,
	"private": true, "unknown": true, "anonymous": true,
	// 测试用串：本身长得像答案（含数字），但作为占位符没有信息量。
	"abcd123": true, "abcd12": true, "abcdef": true,
	"123456": true, "1234567": true, "12345678": true, "password1": true,
}

// 题面里出现这些字样 ⇒ 明确是信封形态。
var envelopeHints = []string{"flag{", "flag {", "FLAG{"}

var envelopeRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]{1,15})\{`)

// DefaultEnvelope 是兜底形态：题面什么都没说时用它。
var DefaultEnvelope = Envelope{Prefix: "flag{", Suffix: "}"}

// Infer 从题面推断本题的答案形态。
//
// 规则（保守优先）：
//  1. 题面出现 "flag{" 之类 ⇒ 信封形态，前缀取自题面。
//  2. 题面出现 `XXX{` 且 XXX 是短标识符 ⇒ 信封形态。
//  3. 题面出现密码/密钥/hash 等字样 ⇒ 允许裸串。
//  4. 两者都没有 ⇒ 默认 flag{...} 信封 **且** 允许裸串（宁可多认，不可漏认；
//     漏认的代价是拿不到分，多认的代价只是一条候选被平台判错）。
func Infer(description string) Shape {
	var s Shape
	seen := map[string]bool{}
	add := func(prefix string) {
		if seen[prefix] {
			return
		}
		seen[prefix] = true
		s.Envelopes = append(s.Envelopes, Envelope{Prefix: prefix, Suffix: "}"})
	}

	low := strings.ToLower(description)
	for _, h := range envelopeHints {
		if strings.Contains(description, h) || strings.Contains(low, strings.ToLower(h)) {
			add(DefaultEnvelope.Prefix)
			break
		}
	}
	// `XXX{` 形态（覆盖 flag{ / ctf{ / KEY{ 等）
	for _, m := range envelopeRe.FindAllStringSubmatch(description, -1) {
		if len(m) > 1 {
			add(m[1] + "{")
		}
	}

	for _, h := range rawHints {
		if strings.Contains(low, strings.ToLower(h)) {
			s.AllowRaw = true
			break
		}
	}

	if len(s.Envelopes) == 0 && !s.AllowRaw {
		// 题面什么都没说：两种形态都认
		s.Envelopes = []Envelope{DefaultEnvelope}
		s.AllowRaw = true
	}
	s.normalize()
	return s
}

// New 直接构造一个只认给定信封的 Shape（测试与显式配置用）。
func New(prefixes ...string) Shape {
	s := Shape{}
	for _, p := range prefixes {
		if !strings.HasSuffix(p, "{") {
			p += "{"
		}
		s.Envelopes = append(s.Envelopes, Envelope{Prefix: p, Suffix: "}"})
	}
	s.normalize()
	return s
}

func (s *Shape) normalize() {
	if s.RawMinLen <= 0 {
		s.RawMinLen = 6
	}
	if s.RawMaxLen <= 0 {
		s.RawMaxLen = 200
	}
}

// norm 返回补齐默认值后的副本。零值 Shape 直接构造（结构体字面量）时
// normalize 不会被调用，所以每个入口都要过一遍。
func (s Shape) norm() Shape {
	s.normalize()
	return s
}

// Empty 表示这个 Shape 不认任何形态（说明题面推断失败）。
func (s Shape) Empty() bool { return len(s.Envelopes) == 0 && !s.AllowRaw }

// MaxEnvelopeLen 是信封载荷的最大长度，用于给抽取正则设上界，避免把一整段
// 输出当成一个 flag。
const MaxEnvelopeLen = 200

// Match 返回 text 中所有符合本题形态的答案候选（已去重、保序）。
func (s Shape) Match(text string) []string {
	s = s.norm()
	if text == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	push := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}

	for _, e := range s.Envelopes {
		re := envelopeMatchRe(e)
		for _, m := range re.FindAllString(text, -1) {
			push(m)
		}
	}
	if s.AllowRaw {
		for _, tok := range rawTokens(text, s.RawMinLen, s.RawMaxLen) {
			push(tok)
		}
	}
	return out
}

// ── 信封正则缓存 ──
//
// 为什么保留缓存（下面是本机实测数字，Broadwell）：一次
// `regexp.MustCompile`（编译本包这个带 `{1,200}` 上界的信封正则）约 100µs /
// 635 次分配 / 98 KB；而把它编译出来的正则拿去扫一条真实的 bash 命令
// （~70 字节）只要 0.4µs。即**编译比使用贵约 250 倍**。所以「干脆不缓存、
// 每次编译」是不可接受的：gate 的每一次 Observe 会对同一条命令文本反复调用
// Match（判候选、判形状、判引号载荷），一道题几百次 Observe 就是几十毫秒纯
// 编译开销，而且每次 98 KB 垃圾 —— 在长驻宿主进程里这会变成 GC 压力。
//
// 但缓存**必须**并发安全。原实现是无锁全局 map，这在 gate 里当前不可达
// （gate.Observe 全程持锁，把所有 Match 串行化了），可 answer 是**导出包**：
// obs（态势台）、多题并行、CLI 并发调用都会直接从包外并发调 Shape.Match /
// LooksLike。Go 的并发 map 写不是「偶尔算错」，是 runtime 直接 fatal
// （concurrent map writes）——宿主是长驻进程（连续跑几十道题不重启），
// 进程一死，正在跑的题、还没落盘的 DAG 全部丢。触发概率随运行时长单调上升，
// 所以「现在不可达」不是不修的理由。见 race_test.go（那条用例从包外并发
// 调用，修前 -race 稳定报 DATA RACE）。
//
// 为什么选 `sync.RWMutex` + 普通 map，而不是 `sync.Map`（实测对比，4 线程
// 并行读、`-cpu 4`）：RWMutex 73ns/op，sync.Map 29ns/op —— sync.Map 的读
// 快 2.5 倍，但绝对值差 44ns。对照上面「编译 100µs」这个量级，44ns 完全在
// 噪声里；而单线程读两者几乎相同（31ns vs 29ns），偏偏 answer 的主要调用方
// gate 就是持锁串行调用的（单线程读是主路径）。用 44ns 换掉 sync.Map 的
// 代价不划算：sync.Map 的 key 集合必须**稳定**（它把被删的 key 推进 dirty
// map 且不回收），而这里的 key 是调用方给的任意前缀、天生无界，正是
// sync.Map 文档不推荐的场景；而且它不能 range、取值要类型断言，正确性不再
// 是一眼可验的。RWMutex 版本只多三行，reviewer 能一眼确认。
var (
	envelopeCacheMu sync.RWMutex
	envelopeCache   = map[string]*regexp.Regexp{}
)

// maxEnvelopeCacheEntries 给这个全局缓存设硬上界。
//
// 为什么要设（而不是「反正信封就 1-2 个」）：key 是 `Prefix + "\x00" + Suffix`，
// Prefix 来自题面（Infer）或调用方（New），**是外部输入**。多题并行时每道题
// 的题面都可能给出新前缀（`FLAG{` / `CTF{` / `KEY{` / 任意短标识符），宿主
// 连续跑几十道题 ⇒ 这个 map 单调增长且永不回收。前身的教训就是全局缓存无界
// 增长，长驻进程 RSS 一路爬升。上界取 64：正常题目信封 1-2 个，跨题复用也
// 到不了两位数，64 个 *Regexp（每个几 KB）合计 < 1 MB；一旦到顶，新前缀就
// 退回「每次编译」，只是慢一点，**绝不失效**（正确性不依赖缓存命中）。
const maxEnvelopeCacheEntries = 64

// envelopeMatchRe 把信封形态编成正则。前缀里的 `{` 等元字符会被转义。
//
// 双检锁（double-check）：先拿读锁查一次，命中就返回（这是热路径，绝大多数
// 调用走这里）；未命中才在锁外编译，再拿写锁查第二次、写入。第二次查是必要
// 的 —— 编译要 100µs，这期间可能有别的 goroutine 已经把同一个 key 填好了，
// 此时必须返回**它**那一份，否则「缓存」名不副实，而且白编译一次。
func envelopeMatchRe(e Envelope) *regexp.Regexp {
	key := e.Prefix + "\x00" + e.Suffix

	envelopeCacheMu.RLock()
	re, ok := envelopeCache[key]
	envelopeCacheMu.RUnlock()
	if ok {
		return re
	}

	inner := "[^" + regexp.QuoteMeta(e.Suffix) + `\s]{1,` + itoa(MaxEnvelopeLen) + "}"
	compiled := regexp.MustCompile(regexp.QuoteMeta(e.Prefix) + inner + regexp.QuoteMeta(e.Suffix))

	envelopeCacheMu.Lock()
	defer envelopeCacheMu.Unlock()
	if re, ok := envelopeCache[key]; ok {
		return re
	}
	if len(envelopeCache) >= maxEnvelopeCacheEntries {
		return compiled // 到顶：不写入，本次直接用编译结果（正确性不受影响）
	}
	envelopeCache[key] = compiled
	return compiled
}

// Contains 报告 text 里是否至少有一个符合形态的答案。
func (s Shape) Contains(text string) bool { return len(s.Match(text)) > 0 }

// LooksLike 报告单个字符串本身是否长得像答案。DAG 的「答案形状内容拒入图」
// 不变量用它：命中即拒收，改路由到 gate 的候选账本。
//
// 与 Match 的区别：Match 从一段长文本里挖候选，LooksLike 判定一个短串的身份。
func (s Shape) LooksLike(v string) bool {
	s = s.norm()
	v = strings.TrimSpace(v)
	if v == "" || len(v) > MaxEnvelopeLen {
		return false
	}
	if len(s.Match(v)) > 0 {
		return true
	}
	if s.AllowRaw && plausibleRaw(v, s.RawMinLen, s.RawMaxLen) {
		return true
	}
	return false
}

// ── 裸串识别 ──

// rawTokenRe 从文本里挖「裸串」候选：不含空白、长度合适的连续可见字符。
var rawTokenRe = regexp.MustCompile(`[A-Za-z0-9!@#$%^&*_\-\.=+/]{6,200}`)

func rawTokens(text string, minLen, maxLen int) []string {
	var out []string
	for _, m := range rawTokenRe.FindAllString(text, -1) {
		if plausibleRaw(m, minLen, maxLen) {
			out = append(out, m)
		}
	}
	return out
}

// rawNoise 是裸串形态下必须排除的高频噪音：命令行片段、路径、常见英文词。
// 前身 B14 的教训是「宁可多拒也不要让噪音挤掉信号」，但这里方向相反——
// 裸串形态下漏认等于丢分，所以只排除**确定不是答案**的东西。
var rawNoise = map[string]bool{
	"http": true, "https": true, "localhost": true, "python": true,
	"static": true, "assets": true, "public": true, "index": true,
	"system": true, "usrbin": true, "binbash": true, "application": true,
}

// assignmentRe 匹配「键 分隔符 值」的形态，例如 `login ==`、`admin: 500`、
// `password=hunter2`、`username: admin`。这类文本是**表单字段名 / 状态码 /
// payload**，不是答案。
//
// 为什么需要它（前身 B14 的真实事故）：凭证正则把 HTML 表单字段（login ==）、
// SQLi payload（password='）、状态码（admin: 500）统统当凭证，某题攒了 61 条
// credential 事实，注入下一场时把真信号挤没。这里的判据是**语法形状**而不是
// 关键词黑名单——`login ==` 之所以是噪音，是因为它长得像赋值而不是值。
//
// 注意 `username: admin` 这种：`user` 是前缀但 `username` 才是键，所以键表里
// 必须显式列出完整键名，否则会退化到「整串求值」而误判。
var assignmentRe = regexp.MustCompile(
	`^[ \t]*(?i:username|user|login|admin|root|password|passwd|pwd|pass|name|` +
		`email|account|token|key|secret|host|hostname|domain|path|url|port|id)` +
		`[ \t]*[:=][ \t]*(.*)$`)

func plausibleRaw(v string, minLen, maxLen int) bool {
	if len(v) < minLen || len(v) > maxLen {
		return false
	}
	if rawNoise[strings.ToLower(v)] {
		return false
	}
	// 「已知键 + 分隔符」形态：只按**值部分**判定。
	// 这样 `password=Admin@123` 仍然可认（值是 Admin@123），而 `login ==`
	// （值空）、`admin: 500`（值是状态码）、`username: admin`（值是角色名）被拒。
	//
	// 键表之外的分隔符（例如 base64 的 `YWJjZA==` 里的 `=`）不走这条路径，
	// 整串求值——否则 base64 答案会被误杀。
	if m := assignmentRe.FindStringSubmatch(v); m != nil {
		return looksLikeRawValue(strings.TrimSpace(m[1]))
	}
	return looksLikeRawValue(v)
}

// looksLikeRawValue 判定一个「值」是否像答案。这是裸串识别的核心。
func looksLikeRawValue(v string) bool {
	if len(v) < 6 {
		return false
	}
	if placeholderValues[strings.ToLower(v)] {
		return false
	}
	hasAlnum := false
	for _, r := range v {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasAlnum = true
			break
		}
	}
	if !hasAlnum {
		return false
	}
	hasDigit, hasUpper, hasLower, hasSymbol := false, false, false, false
	for _, r := range v {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case r == '_':
			// 下划线是标识符字符，不是「像密钥」的信号。
			// 否则 `enable_queue` 这种纯小写下划线串会被当成答案（前身 B14 噪音）。
		default:
			hasSymbol = true
		}
	}
	if hasDigit || hasSymbol {
		return true
	}
	// 纯字母：只有「大小写混合且长度 >= 10」才认（camelCase 风格的密钥）
	return hasUpper && hasLower && len(v) >= 10
}

// itoa 是个小工具，避免为一个整数格式化引入 strconv 到热路径。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
