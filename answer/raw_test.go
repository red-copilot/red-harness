package answer

import "testing"

// 这些用例钉住的是「裸串形态下的边界」——它比信封形态更容易出错，因为
// 判定依据是「像不像一个值」，而不是一个明确的语法。
//
// 每一条都对应一个真实场景：漏认 = 丢分，误认 = 噪音挤掉信号（前身 B14）。

func TestAssignmentFormsRejected(t *testing.T) {
	s := Shape{AllowRaw: true}
	// 键=值 形态但「值」不像答案 ⇒ 拒
	for _, v := range []string{
		"login ==", "pass ==", "admin ==", // 值为空
		"login: 500", "admin: 500", // 值是 HTTP 状态码
		"password='", "passwd=\"", // SQLi payload 的残片
		"username: admin",    // 值是角色名
		"token: undefined",   // 占位符
		"password: password", // 自指
	} {
		if s.LooksLike(v) {
			t.Errorf("%q 不应被认作答案", v)
		}
	}
}

func TestAssignmentFormsAccepted(t *testing.T) {
	s := Shape{AllowRaw: true}
	// 键=值 且「值」本身像个答案 ⇒ 认（前身的真凭证样本）
	for _, v := range []string{
		"password=Admin@123",
		"passwd:hunter2xyz",
		"pwd=john.doe",
	} {
		if !s.LooksLike(v) {
			t.Errorf("%q 应被认作答案", v)
		}
	}
}

func TestIdentifierLikeRejected(t *testing.T) {
	s := Shape{AllowRaw: true}
	// 纯小写 + 下划线的标识符不是答案
	for _, v := range []string{"enable_queue", "get_user_by_id", "parse_config_file"} {
		if s.LooksLike(v) {
			t.Errorf("标识符 %q 不应被认作答案", v)
		}
	}
}

func TestCamelCaseKeyAccepted(t *testing.T) {
	s := Shape{AllowRaw: true}
	if !s.LooksLike("hunterTwoXyz") {
		t.Error("大小写混合的长串应被认作密钥形态")
	}
}

func TestHexHashAccepted(t *testing.T) {
	s := Shape{AllowRaw: true}
	// md5 / sha1 形态
	for _, v := range []string{
		"5d41402abc4b2a76b9719d911017c592",
		"aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d",
	} {
		if !s.LooksLike(v) {
			t.Errorf("hash %q 应被认作答案", v)
		}
	}
}

func TestEnvelopeOnlyShapeRejectsRaw(t *testing.T) {
	s := Shape{Envelopes: []Envelope{{Prefix: "flag{", Suffix: "}"}}}
	if s.LooksLike("Admin@123") {
		t.Error("只认信封的 Shape 不应接受裸串")
	}
	if !s.LooksLike("flag{ok}") {
		t.Error("只认信封的 Shape 应接受信封")
	}
}

func TestMatchMixedContent(t *testing.T) {
	s := Shape{Envelopes: []Envelope{{Prefix: "flag{", Suffix: "}"}}, AllowRaw: true}
	text := "found flag{real_one} and also password=Admin@123 in config"
	got := s.Match(text)
	want := map[string]bool{"flag{real_one}": true, "password=Admin@123": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d candidates", got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("意外候选 %q", g)
		}
	}
}
