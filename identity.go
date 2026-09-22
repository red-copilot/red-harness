package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// ── 部署身份：owner / daemon endpoint ──
//
// 这一节是「锁与回收作用域对齐」的接口冻结面。它修的是一个**作用域不匹配**的
// 真实缺陷：互斥锁过去落在 `<StoreDir>/run.lock`（身份随 StoreDir 变），而孤儿
// 回收扫的是**宿主全局**的 Docker label（作用域与 StoreDir 无关）。于是两个用了
// 不同 `--store` 的进程各自拿到锁、各自把对方认成孤儿，然后 `docker rm --force`
// 掉对方**正在用**的容器、网络与 iptables 规则。当前锁真正保护的只有「同
// StoreDir」这一种部署，而那**不是**它声称保护的东西。
//
// 修法是让两者同源：**锁身份 = daemon endpoint，资源归属 = owner**。两者都在
// 这里解析一次，由装配层与 executor 共用，不再各算各的。

// OwnerID 标识「哪个部署主体创建了这个资源」，是资源标签
// `red-harness.owner` 的值。
//
// 它是删除闸门的输入：回收只处理 owner **完全匹配**的资源。
//
// **为什么删除不能只靠「我持有锁」**：`HarnessOptions.Locker` 与 `Sandbox`
// 都是公开端口，`Reclaim`/`ReclaimStale` 都是公开方法。一个注入 no-op locker
// 的调用方，会凭一个**无人验证的声明**去删资源。锁是第一道门，owner 是第二道
// 门；两道门的失效模式不同（锁失效是「作用域算错」，owner 失效是「身份算错」），
// 所以两道都要有。
type OwnerID string

// OwnerUnknown 表示资源上**没有** owner label（升级前的旧资源），或标签值为空。
//
// ⚠️ 它的语义是「**无法证明归属**」，不是「属于某个未知的人」。回收遇到它
// **只报告、绝不删除**——把「证明不了是我的」当成「可以删」正是这个缺陷的形态。
const OwnerUnknown OwnerID = ""

// ResolveOwner 由 hostname 与 uid 派生 owner。**纯函数**：不读全局状态，
// 便于测试与核对。
//
// hostname 为空、或 sanitize 后为空 ⇒ KindConfig。这是 **fail closed**：
// 归属是删除闸门，不能靠一个假值兜底——一个空 owner 会让「只删自己的」
// 退化成「谁的都删」，也就是把这次修复原样取消。
func ResolveOwner(hostname string, uid int) (OwnerID, error) {
	h := sanitizeLabelValue(hostname)
	if h == "" {
		return OwnerUnknown, Ef(KindConfig, "harness.owner",
			"无法确定本机 hostname，资源归属不可判定；请显式指定 owner", nil)
	}
	return OwnerID(h + "/" + strconv.Itoa(uid)), nil
}

// Fingerprint 是 owner 的 8 位十六进制摘要，供**长度敏感的落点**使用：
// iptables 规则注释要求 ≤256 字符且无引号 / 空白 / 换行，而 owner 的值域
// 来自 hostname（可能很长）。
//
// **为什么由这里提供、而不是让使用者自己截断**：同一个身份的第二处截断必然
// 与第一处漂移，而漂移的形态是「两条规则看起来都像本 owner 的」——静默的。
// 与 `answer.Fingerprint` 是同一条纪律。
func (o OwnerID) Fingerprint() string {
	if o == OwnerUnknown {
		return ""
	}
	sum := sha256.Sum256([]byte(o))
	return hex.EncodeToString(sum[:])[:8]
}

// sanitizeLabelValue 把任意串收进 label 值的安全字符集 `[A-Za-z0-9._-]`，
// 其余字符替换成 `_`。与仓库里其它字符集闸同向：**公开面只放穷举过的形态**。
func sanitizeLabelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// DockerEndpoint 是**规范化后**的 daemon 身份。
//
// 它**不是**用户输入字符串的转发：`tcp://user:pass@host:2375` 这种值可以带
// 凭据，而规范化后的 ID 只由 scheme 与本地绝对路径拼成，既不含凭据也不会把
// 两个不同的输入当成同一个 daemon。
//
// 身份参与的落点有两处，都要求「拒绝发生在任何副作用之前」：
// 锁路径（`Fingerprint`）与 docker 子进程的 `DOCKER_HOST`（`ID`）。
type DockerEndpoint struct {
	source  string // "DOCKER_HOST" / "DOCKER_CONTEXT" / "default"
	scheme  string // 目前恒为 "unix"
	host    string // 本地绝对路径
	context string // 生效的 context 名（空 = default）
}

const (
	// DefaultDockerSocket 是本机 daemon 的默认 socket。
	DefaultDockerSocket = "/var/run/docker.sock"
	// DefaultDockerEndpointID 是默认端点的规范化 ID。
	DefaultDockerEndpointID = "unix://" + DefaultDockerSocket
)

// DefaultDockerEndpoint 返回默认本机端点。
func DefaultDockerEndpoint() DockerEndpoint {
	return DockerEndpoint{source: "default", scheme: "unix", host: DefaultDockerSocket}
}

// ID 是规范化端点串（`unix://<绝对路径>`）。它进子进程环境变量，不进公开面。
func (e DockerEndpoint) ID() string { return e.scheme + "://" + e.host }

// IsLocal 报告这个端点是否属于本机。当前**只有本机端点被支持**：
// 宿主级锁无法跨主机互斥，所以远程 daemon 必须拒绝而不是「尽力而为」。
func (e DockerEndpoint) IsLocal() bool { return e.scheme == "unix" }

// Source 说明这个身份是从哪个环境变量解析出来的，只进错误消息与 doctor。
func (e DockerEndpoint) Source() string { return e.source }

// Fingerprint 是端点 ID 的 8 位十六进制摘要，作为**锁文件名**的区分位。
//
// 为什么要按端点区分而不是一把全局锁：同一个宿主上两个**不同的**本地 socket
// 是两个不同的 daemon，它们不共享容器、网络或 iptables 表，本就不该互斥；
// 用一把锁会把它们串行化，那是把「作用域算错」换成另一个方向的算错。
// 默认端点（`unix:///var/run/docker.sock`）的指纹是常量 `13c4025c`，
// 所以默认部署下锁文件名是确定的。
func (e DockerEndpoint) Fingerprint() string {
	sum := sha256.Sum256([]byte(e.ID()))
	return hex.EncodeToString(sum[:])[:8]
}

// ResolveDockerEndpoint 从环境解析并**规范化** daemon 身份。
//
// **纯函数**：只读传入的 env 取值函数，**不连 daemon**。「daemon 在不在」是
// doctor 的事，与「身份是什么」正交——把两者混起来会让「身份不可判定」与
// 「daemon 没起」变成同一类失败，而前者重试无意义。
//
// 判据（逐条）：
//
//	DOCKER_HOST 空 + DOCKER_CONTEXT 空或 "default" ⇒ 默认端点
//	DOCKER_HOST = unix://<绝对路径>                 ⇒ 该路径（不同的本地 socket
//	                                                   是不同 daemon ⇒ 不同锁）
//	DOCKER_HOST = unix://<相对路径> 或空路径         ⇒ KindConfig
//	DOCKER_HOST 为 tcp/http/https/ssh/npipe/fd 等   ⇒ KindConfig（远程不受支持）
//	DOCKER_HOST 非空但无法解析                       ⇒ KindConfig
//	DOCKER_HOST 空 + DOCKER_CONTEXT 非 default       ⇒ KindConfig
//
// `DOCKER_CONTEXT` 非默认时为什么拒绝而不是解析：context 指向哪个 daemon 存在
// `~/.docker/config.json` 里，**在不起 daemon 的前提下无法离线证明**，而我们要
// 用它当锁身份。拒绝并提示用 `DOCKER_HOST` 显式指定本机 socket。
//
// ⚠️ 错误消息**不回显原始取值**：`DOCKER_HOST` 可以是
// `tcp://user:pass@host:2375`，而 `Error.Msg` 会进公开结果。消息里只报 scheme。
func ResolveDockerEndpoint(env func(string) string) (DockerEndpoint, error) {
	host := strings.TrimSpace(env("DOCKER_HOST"))
	ctx := strings.TrimSpace(env("DOCKER_CONTEXT"))

	if host == "" {
		if ctx != "" && !strings.EqualFold(ctx, "default") {
			return DockerEndpoint{}, Ef(KindConfig, "harness.docker_endpoint",
				"DOCKER_CONTEXT 指向哪个 daemon 无法在不起 daemon 的前提下验证；"+
					"请 unset DOCKER_CONTEXT 并用 DOCKER_HOST 显式指定本机 unix socket", nil)
		}
		ep := DefaultDockerEndpoint()
		if ctx != "" {
			ep.source = "DOCKER_CONTEXT"
			ep.context = ctx
		}
		return ep, nil
	}

	scheme, rest, ok := strings.Cut(host, "://")
	if !ok {
		return DockerEndpoint{}, Ef(KindConfig, "harness.docker_endpoint",
			"DOCKER_HOST 无法解析为 <scheme>://<path>；本机 unix socket 之外不受支持", nil)
	}
	scheme = strings.ToLower(scheme)
	if scheme != "unix" {
		return DockerEndpoint{}, Ef(KindConfig, "harness.docker_endpoint",
			"检测到非 unix 端点（scheme="+sanitizeLabelValue(scheme)+"）：远程 docker daemon "+
				"不受支持，宿主级锁无法跨主机互斥", nil)
	}
	if !strings.HasPrefix(rest, "/") {
		return DockerEndpoint{}, Ef(KindConfig, "harness.docker_endpoint",
			"DOCKER_HOST 的 unix 路径必须是绝对路径；相对路径会让锁身份随工作目录变化", nil)
	}
	return DockerEndpoint{source: "DOCKER_HOST", scheme: "unix", host: rest}, nil
}

// ── 题目身份 ──

// ChallengeID 是平台下发的题目编号（`Challenge.Code`）映射出的**路径安全、
// 标签安全**的稳定 ID：小写十六进制 sha256，64 字符。
//
// 它存在的理由是一条既有纪律：**平台下发的 code 不得成为路径段**（平台数据
// 出现在文件系统路径上会把「编号」变成「路径控制」）。过去这条规则只是
// `store/trace.go` 里的一行内联 `sha256`，而它现在有两个消费者——store 的产物
// 路径与 executor 的资源标签——**而 store 与 executor 是两两互不 import 的
// 实现包**。所以唯一真源只能在本包：否则第二个消费者必然抄一份，抄出来的两份
// 是「同一个名字下的两种值」，错配是静默的。
type ChallengeID string

// ChallengeIDFor 把题目编号映射成 ChallengeID。
//
// 空串或全空白 ⇒ KindConfig：**入口就拒绝**，不让一个空 ID 变成
// `runs/<id>//graph.json` 这种把布局悄悄改掉的路径。
//
// ⚠️ 只判空，**不做 trim 后再哈希**：规范化会静默地把 `" a "` 与 `"a"` 变成
// 同一个身份，而平台真的下发过带空白的编号——那两种是不同题目还是同一题，
// 不是宿主该替平台决定的事。
func ChallengeIDFor(code string) (ChallengeID, error) {
	if strings.TrimSpace(code) == "" {
		return "", Ef(KindConfig, "harness.challenge_id",
			"题目编号为空，无法生成产物路径或资源标签", nil)
	}
	sum := sha256.Sum256([]byte(code))
	return ChallengeID(hex.EncodeToString(sum[:])), nil
}

// ValidChallengeID 报告 s 是否是 ChallengeIDFor 的合法产物。
//
// 读取侧用它做**严格**校验：只接受 64 位小写十六进制。任何调用方给的串
// （含从旧文件读回来的）都要先过这里，否则一个 `../..` 就能把路径带走。
func ValidChallengeID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// AttemptID 是一题的第几次尝试。
type AttemptID int

// FirstAttempt 是本轮**唯一**的 attempt 号。
//
// ⚠️ **它不是一个计数器。** N0 没有重试：每题一个 sandbox、一个 session、
// 跑一次。保留 `attempts/<n>` 这一层路径，是为了让将来引入重试时**不需要
// 迁移任何已存在的 run 目录**，而不是为了让这里看起来像个能变的量。
//
// 为什么不干脆省略这一层、等有重试了再加：那样 reader 就要同时认「两层」
// 与「三层」两种目录形态（旧路径兼容已经要处理一种了）。代价是一个空目录层，
// 收益是零迁移。**但绝不要给它加分支**——一个永远为 1 的计数器在 schema 里
// 就是一句谎话，读的人会据此写出恢复逻辑。
const FirstAttempt AttemptID = 1

// ── 一次部署的完整身份 ──

// ExecutionIdentity 是一次运行所属的**部署身份**：谁（Owner）在哪个 daemon
// （Endpoint）上执行。
//
// 两者必须**同源解析一次**再分发给各自的消费者：owner 进资源标签、endpoint
// 进锁路径与 docker 子进程环境。分开解析会出现「锁按 A 端点、标签按 B 端点」，
// 而那种错配的表现是「回收删不掉自己的资源，或删掉了别人的」。
type ExecutionIdentity struct {
	Owner    OwnerID
	Endpoint DockerEndpoint
}

// ResolveExecutionIdentity 解析一次部署身份。纯函数，不连 daemon、不读全局状态。
func ResolveExecutionIdentity(env func(string) string, hostname string, uid int) (ExecutionIdentity, error) {
	ep, err := ResolveDockerEndpoint(env)
	if err != nil {
		return ExecutionIdentity{}, err
	}
	owner, err := ResolveOwner(hostname, uid)
	if err != nil {
		return ExecutionIdentity{}, err
	}
	return ExecutionIdentity{Owner: owner, Endpoint: ep}, nil
}
