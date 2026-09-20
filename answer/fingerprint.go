package answer

// 本文件是**答案指纹的唯一真源**。
//
// ── 为什么要统一（核验发现的事故）──
//
// 核验在 gate 与 dag 里各找到一份指纹实现，**格式不同**：
//
//	gate.Fingerprint       = `<hex8>:<字符数>:<首><尾>`     例：6326baf8:24:f}
//	dag.FlagFingerprint    = `fp:<hex8>/len=<N>/<首>…<尾>`  例：fp:6326baf8/len=24/f…}
//
// 两者算的哈希**是同一个**（都是 sha256 前 8 位十六进制），但拼出来的串不同。
// 后果不是「显示不好看」，是**跨包对照无法 join**：gate 的判错账本（
// Ledger.byFP / formatRejectFP）用 gate 格式做键，dag 回灌进 prompt 的
// Rejected 列表用 dag 格式，报告层想把「gate 说平台判错的」和「dag 记下的
// 死胡同」对上时，字符串比不相等，只能各自为政 —— 于是同一份「判错回灌」
// 在两条链路上各记一套，重复占用 prompt 预算，而设计文档明确要求判错回灌
// **只给指纹**（前身 B52：明文回灌 = 把答案又塞回上下文，一旦上下文被压缩 /
// 落盘 / 上报就泄漏）。两套格式让这条要求在最需要它的地方（关联去重）失效。
//
// 所以函数移到这里：answer 是**最底层**的包，gate 与 dag 都依赖它，把真源
// 放在这里，两边谁都不用依赖对方（简报纪律 3：gate 不得依赖 dag，反之亦然）。

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Fingerprint 返回一个答案的指纹，格式：
//
//	fp:<sha256[:8]>/len=<字符数>/<首字符>…<尾字符>
//
// 例：`Fingerprint("flag{super_secret_value}")` → `fp:6326baf8/len=24/f…}`
//
// ── 为什么是这个格式（三个字段各解决一个问题）──
//
//  1. `fp:` 前缀。指纹会出现在**纯文本**里：回灌进下一轮 prompt 的 Rejected
//     段、报告文件、日志。没有前缀时它是一串裸十六进制，人和模型都认不出
//     「这是什么」，也无从在长文本里 grep。有前缀就能一眼识别、能被
//     `grep -o 'fp:[0-9a-f]\{8\}'` 精确捞出来。
//  2. `<sha256[:8]>` 给碰撞空间。8 位十六进制 = 32 bit，对「一道题里判错的
//     几个到几十个候选」这个量级，碰撞概率可忽略；同时又短到能塞进 prompt。
//     它的作用是**区分不同 flag**：不同答案必须得到不同指纹，否则账本会把
//     两个不同的错误答案当成同一个（后果：真答案被误当「已判错」而放弃）。
//  3. `len=<N>/<首>…<尾>` 给**人工核对**能力。只有哈希时，人（或 agent）
//     看到一个指纹完全无法判断它是不是自己刚提交的那个；加上长度和首尾
//     字符，`fp:6326baf8/len=24/f…}` 与「我提交的 flag{...} 是 24 个字符、
//     以 f 开头 } 结尾」可以手工对上号 —— 这正是「判错回灌告诉 agent
//     『这个不要再试』」能起作用的前提。
//
// 长度与首尾用**字符数 / 字符**而不是字节数 / 字节：中文等非 ASCII 答案
// 按字节取首尾会切出半个字符（UTF-8 中间字节），渲染出来是乱码，人也没法
// 核对；长度按字节算还会虚高（前身按字符数记账）。所以用 RuneCount /
// DecodeRune / DecodeLastRune。
//
// ── 绝不泄漏明文 ──
//
// 这是硬约束（前身 B52 / `_scrub_flag_plaintext` 的事故：flag 明文泄漏进了
// 会回灌下一场的文件 —— MEMORY.md / _blackboard.json / tried_commands.md）。
// 本函数只输出：哈希前 8 位（单向）、长度、**两个字符**。首尾各一个字符
// 不足以反推明文（24 字符的 flag 只暴露 2 个字符），却足够人核对。注意
// 「首尾字符」在极端短答案（len=1）上等于暴露该答案本身，但长度 1 的串
// 本来就不可能是指纹化的答案（Shape.RawMinLen 默认 6），且此时暴露的
// 1 个字符等于长度信息，没有额外损失。
//
// 稳定性：纯函数，无状态、无 map、无时间 —— 同一答案在任何进程、任何
// 时刻、任何包调用都得到同一串（跨进程重启后账本仍能 join）。
//
// ── 调用方迁移 ──
//
// **gate/dag 应改为调用本函数**，删掉各自的本地实现（gate/provenance.go 的
// `Fingerprint`、dag/store.go 的 `FlagFingerprint`），格式统一为：
//
//	fp:<hex8>/len=N/<首>…<尾>
//
// 本包只提供函数，不动那两个包的调用点（由仓库所有者统一改）。迁移时注意
// gate 的 `TestFingerprintHidesPlaintext` / `TestFingerprintCountsRunes` 与
// dag 的 `TestFlagFingerprintNoPlaintext` 里对**格式**的断言需要同步更新
// （它们钉的是旧格式的字段切分，语义不变，换成本函数的格式即可）。
//
// 选这一套（dag 那套）而不是 gate 那套的理由：`fp:` 前缀便于在文本里识别
// （上面第 1 点）；`len=` 比裸冒号可读 —— `<hex8>:24:f}` 里 `24` 是什么，
// 不查实现根本猜不到，而 `len=24` 自解释。两边的用途它都满足：gate 拿它
// 做判错账本的**键**（纯函数 ⇒ 键稳定，join 可靠），dag 把它渲染进 prompt
// 给 agent 看（前缀 + 长度 + 首尾，人和模型都能核对）。
func Fingerprint(s string) string {
	s = strings.TrimSpace(s)
	sum := sha256.Sum256([]byte(s))
	fp := hex.EncodeToString(sum[:])[:8]

	// 空串单列：没有首尾字符可取，`len=0` 已经说明一切。
	// （空串不是合法答案，但账本可能收到它 —— gate.Ledger.Record 对空串
	// 直接 return，这里仍要给出确定结果，避免调用方拿到畸形串。）
	if s == "" {
		return "fp:" + fp + "/len=0"
	}

	r := []rune(s)
	first, last := string(r[0]), string(r[len(r)-1])
	// len=1 时首尾是同一个字符，写两遍没有信息量，也让「首…尾」看起来像
	// 两个字符 —— 直接写同一个字符，读的人不会误以为答案更长。
	if len(r) == 1 {
		last = first
	}
	// 长度用 len(r)（即字符数）：已经为取首尾建了 []rune，再调
	// utf8.RuneCountInString 只会白扫一遍。
	return "fp:" + fp + "/len=" + strconv.Itoa(len(r)) + "/" + first + "…" + last
}
