package answer_test

import (
	"strings"
	"testing"

	"github.com/red-copilot/red-harness/answer"
)

// ── 指纹：格式、稳定性、区分度、绝不泄漏明文 ──
//
// 这些用例钉的是「跨包 join」这个具体用途：gate 用指纹做判错账本的键，dag 把
// 指纹渲染进 prompt 给 agent 看，报告层要能把两边的记录按字符串对上。所以
// 「同一答案在任何地方得到同一串」和「不同答案必须不同」都是**契约**，不是
// 实现细节。

// 语料：一个含明文标记的 flag。断言里专门查 super_secret 是否出现 ——
// 前身 B52 的真实事故是 flag 明文泄漏进了会回灌下一场的持久文件
// （MEMORY.md / _blackboard.json / tried_commands.md）。
const secretFlag = "flag{super_secret_value}"

func TestFingerprintFormat(t *testing.T) {
	fp := answer.Fingerprint(secretFlag)
	// 格式契约：fp:<hex8>/len=N/<首>…<尾>
	if !strings.HasPrefix(fp, "fp:") {
		t.Fatalf("指纹必须以 fp: 开头（便于在 prompt / 报告文本里识别），got %q", fp)
	}
	parts := strings.Split(fp, "/")
	if len(parts) != 3 {
		t.Fatalf("指纹格式应为 fp:<hex8>/len=N/<首>…<尾>，got %q", fp)
	}
	hex8 := strings.TrimPrefix(parts[0], "fp:")
	if len(hex8) != 8 {
		t.Errorf("哈希前缀应为 8 位十六进制，got %q", hex8)
	}
	for _, c := range hex8 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("哈希前缀含非十六进制字符 %q: %q", c, hex8)
		}
	}
	// "flag{super_secret_value}" 是 24 个字符，首 f 尾 }
	if parts[1] != "len=24" {
		t.Errorf("长度字段应为 len=24（按字符数），got %q", parts[1])
	}
	if parts[2] != "f…}" {
		t.Errorf("首尾字段应为 f…}，got %q", parts[2])
	}
	// 钉死哈希值：换实现（例如换哈希算法 / 换截断长度）会让跨进程 join 失败，
	// 这个断言就是那道警报。值 = sha256("flag{super_secret_value}")[:8]。
	if hex8 != "6326baf8" {
		t.Errorf("哈希前缀应为 6326baf8（sha256 前 8 位），got %q", hex8)
	}
}

// 指纹里**绝不能**出现明文子串 —— 这是硬约束，不是「最好没有」。
func TestFingerprintNeverLeaksPlaintext(t *testing.T) {
	for _, s := range []string{
		secretFlag,
		"flag{super_secret_value_2}",
		"CTF{super_secret_value}",
		"password=super_secret_value",
		"super_secret_value",
	} {
		fp := answer.Fingerprint(s)
		if strings.Contains(fp, "super_secret") {
			t.Errorf("指纹泄漏明文片段 %q: %q", s, fp)
		}
		if strings.Contains(fp, s) {
			t.Errorf("指纹包含整个明文 %q: %q", s, fp)
		}
	}
	// 连续片段也查一遍：首尾各一个字符是刻意保留的（人核对用），但明文中间
	// 的任何**连续片段**都不该出现。用明文内部的长片段做断言，避免单字符
	// 巧合（例如 'e' 本来就出现在 "fp"/"len" 里）造成假警报。
	const inner = "super_secret_valu"
	if fp := answer.Fingerprint("flag{" + inner + "e}"); strings.Contains(fp, inner) {
		t.Errorf("指纹含明文内部连续片段: %q", fp)
	}
	if fp := answer.Fingerprint("flag{super_secret_value}"); strings.Contains(fp, "super_secret") {
		t.Errorf("指纹含明文内部连续片段: %q", fp)
	}
}

// 同一答案必须稳定（跨调用、跨进程 —— 纯函数，无 map、无时间）。
func TestFingerprintStable(t *testing.T) {
	a := answer.Fingerprint(secretFlag)
	b := answer.Fingerprint(secretFlag)
	if a != b {
		t.Fatalf("同一答案的指纹必须稳定: %q vs %q", a, b)
	}
	// 前后空白不影响（TrimSpace 归一）—— gate / dag 都先 trim 再记账，
	// 若这里不 trim，同一答案因尾随换行会得到两个不同指纹，账本去重失效。
	if answer.Fingerprint("  "+secretFlag+"\n") != a {
		t.Errorf("首尾空白应被归一，got %q", answer.Fingerprint("  "+secretFlag+"\n"))
	}
}

// 不同答案必须得到不同指纹 —— 否则账本会把两个错误答案当成同一个，
// 真答案可能被误判成「已试过」而放弃。
func TestFingerprintDistinguishes(t *testing.T) {
	cases := []string{
		"flag{a}", "flag{b}",
		"flag{super_secret_value}", "flag{super_secret_value2}",
		"flag{super_secret_value}", "flag{other_value}",
		// 长度相同、首尾相同，只有中间不同：这种最容易被弱哈希混同。
		"flag{aaaa1}", "flag{aaaa2}",
	}
	seen := map[string]string{}
	for _, c := range cases {
		fp := answer.Fingerprint(c)
		if prev, ok := seen[fp]; ok && prev != c {
			t.Errorf("指纹碰撞：%q 与 %q 都得到 %q", prev, c, fp)
		}
		seen[fp] = c
	}
}

// 长度与首尾按**字符数 / 字符**而不是字节数 / 字节：非 ASCII 答案按字节取
// 首尾会切出半个 UTF-8 字符（渲染成乱码，人也无法核对），长度还会虚高。
func TestFingerprintCountsRunes(t *testing.T) {
	// "密码abc" = 5 个字符（3 个汉字 + 2 个字母），字节数是 3*3+2 = 11。
	fp := answer.Fingerprint("密码abc")
	if !strings.Contains(fp, "len=5") {
		t.Errorf("长度应按字符数（5），got %q", fp)
	}
	if !strings.HasSuffix(fp, "密…c") {
		t.Errorf("首尾应是完整字符 密…c，got %q", fp)
	}
	if !strings.Contains(fp, "/len=") || !strings.Contains(fp, "…") {
		t.Errorf("格式字段缺失: %q", fp)
	}
}

// 单字符答案：首尾是同一个字符，不写两遍（否则读的人会误以为答案更长）。
func TestFingerprintSingleRune(t *testing.T) {
	fp := answer.Fingerprint("中")
	if !strings.Contains(fp, "len=1") {
		t.Errorf("长度应为 1，got %q", fp)
	}
	if !strings.HasSuffix(fp, "中…中") {
		t.Errorf("单字符答案首尾应相同，got %q", fp)
	}
}

// 空串：没有首尾可取，只给 len=0（不能产出畸形串让调用方 panic）。
func TestFingerprintEmpty(t *testing.T) {
	fp := answer.Fingerprint("")
	if !strings.Contains(fp, "/len=0") {
		t.Errorf("空串指纹应含 len=0，got %q", fp)
	}
	if strings.Contains(fp, "…") {
		t.Errorf("空串没有首尾字符，不该出现 …，got %q", fp)
	}
	if fp != answer.Fingerprint("   ") {
		t.Errorf("纯空白应等同于空串，got %q vs %q", fp, answer.Fingerprint("   "))
	}
}

// 指纹本身可以直接当 map 键 / 落盘 / 塞进 prompt：不含空白、不含换行，
// 也不会因为长度不同而在文本里「看起来像两个字段」。
func TestFingerprintIsTextSafe(t *testing.T) {
	for _, s := range []string{secretFlag, "密码abc", "a", ""} {
		fp := answer.Fingerprint(s)
		if strings.ContainsAny(fp, " \t\r\n") {
			t.Errorf("指纹含空白字符（会破坏 prompt 里的分行渲染）: %q", fp)
		}
	}
}
