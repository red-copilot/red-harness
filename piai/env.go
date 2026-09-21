package piai

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadEnv 从 start 目录起**向上逐级查找** `.env`，返回第一个命中的文件的
// 键值对、它的绝对路径、以及错误。找不到 `.env` 时返回空 map（不是错误）——
// 「没有凭据文件」是合法状态，调用方可能完全靠进程环境变量工作。
//
// 为什么必须向上找：`go run ./example` 与 `go run ./cmd/redcopilot` 的工作目录
// 不同（example/ 与 cmd/redcopilot/），而 .env 只放在仓库根。前身固定读
// "./.env" 的后果不是报错，而是**静默**：pi 拿不到 OPENCODE_API_KEY 就退回默认
// provider，401 被 pi 呈现成一次空会话（M0 实测：stopReason=error + 空 content +
// 照样 agent_settled），整个题库以「跑完了但什么都没发生」的形式被烧掉。
// 向上查找把这种静默失效变成「只要在仓库里就一定能读到」。
func LoadEnv(start string) (map[string]string, string, error) {
	if strings.TrimSpace(start) == "" {
		start = "."
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, "", err
	}
	for dir := abs; ; {
		p := filepath.Join(dir, ".env")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			m, err := parseEnvFile(p)
			if err != nil {
				return nil, p, err
			}
			return m, p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // 已到根，停。filepath.Dir("/") == "/" 是唯一的终止条件。
		}
		dir = parent
	}
	return map[string]string{}, "", nil
}

// parseEnvFile 是原 pi/env.go 的 loadEnv：解析 KEY=VALUE，跳过空行与注释，
// 去掉可选的 `export ` 前缀与成对的单/双引号。
//
// 有意**不做**变量展开（`${FOO}` 原样保留）。展开会引入「.env 里的值依赖进程
// 环境」的隐式行为，而这里读出来的东西是要注入子进程的，两处展开顺序不同
// 就会得到不同的值——宁可把不展开这件事写死在注释里。
func parseEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			m[k] = v
		}
	}
	return m, nil
}

// childEnv 构造 pi 子进程的环境：进程环境 **覆盖** .env（CLI flag > 环境变量 >
// .env > 默认值，见设计文档 §七），并把 binDir 前置进 PATH。
//
// PATH 前置不是可选项：pi 是个 `#!/usr/bin/env node` 的脚本，而系统 node 是 v18
// （M0 实测跑不起来）。bundled 的 node 22 就在 pi 的同级目录里，只有把它放到
// PATH 最前面，`env node` 才会解析到它。
func childEnv(envMap map[string]string, binDir string) []string {
	merged := map[string]string{}
	for k, v := range envMap {
		merged[k] = v
	}
	// 进程环境后写 ⇒ 覆盖 .env。
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	if binDir != "" {
		merged["PATH"] = binDir + string(os.PathListSeparator) + merged["PATH"]
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// sandboxEnv 构造 sandbox 主进程的环境。它**刻意不继承 os.Environ**：容器里
// 只需要 provider 凭据与最小运行环境，平台 token 与宿主控制变量必须留在可信
// 一侧（PLAN v0.4：模型凭据仅注入 sandbox 主进程环境）。
//
// 三条纪律：
//
//  1. PATH 固定成镜像里 pi 实际所在的目录集合。它不能从宿主继承：宿主 PATH 在
//     容器里指向一堆不存在的路径，而 pi 是 `#!/usr/bin/env node` 脚本，PATH 错
//     了就找不到 node（runner 镜像里 node 与 pi 都在 /usr/local/bin）。
//  2. 凭据只经**进程环境**传递，绝不进命令行（argv 在宿主上 `ps` 可见）。这里
//     只往 env 里写，argv 由 buildArgs 构造，两者不交叉。
//  3. HOME 必须落在容器内**可写**的位置（见 sandboxHome）。
//
// home 为空时用 /work/.home：与 executor 的缺省 HOME（<workdir>/.home）一致。
// 调用方（Agent.Start）在 sandbox 路径下总是传入 a.sandboxHome()，这里的兜底只
// 针对直接调用 sandboxEnv 的场景。
func sandboxEnv(envMap map[string]string, provider, home string) []string {
	merged := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"}
	for k, v := range envMap {
		merged[k] = v
	}
	// 进程环境**补位**：.env 里有的以 .env 为准（显式配置优先），进程环境只补
	// .env 没有的。注入的名字只限 provider 凭据族——不是「把宿主环境搬进去」。
	for _, name := range providerCredentialNames(provider) {
		if merged[name] != "" {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			merged[name] = v
		}
	}
	if home != "" {
		merged["HOME"] = home
	} else if merged["HOME"] == "" {
		merged["HOME"] = defaultContainerWorkdir + "/" + containerHomeName
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// providerCredentialNames 返回某个 provider 可能使用的凭据环境变量名。
//
// 已知别名（opencode-go 走 OPENCODE_API_KEY）与惯例名一起给出，顺序固定
// （惯例名在前）。这里返回的**只有名字，没有值**，可以安全地进日志/错误消息。
//
// provider 为空时只给别名族：惯例名会退化成 "_API_KEY" 这个荒谬的名字。
// 曾经的写法是 `if name != "_API_KEY"` 这样一个字符串比较守卫——它把「空
// provider 推导出 _API_KEY」这个真实缺陷藏起来了：守卫与缺陷同源，一旦将来
// 有人把守卫删掉（或把别名列表改掉），空 provider 就会去读宿主的 "_API_KEY"
// 环境变量并注入容器。修法是不让这个名字被生成出来，而不是把它过滤掉。
func providerCredentialNames(provider string) []string {
	var names []string
	if strings.TrimSpace(provider) != "" {
		names = append(names, providerKeyName(provider))
	}
	switch provider {
	case "opencode-go", "opencode":
		names = append(names, "OPENCODE_API_KEY", "OPENCODE_GO_API_KEY")
	case "anthropic":
		names = append(names, "ANTHROPIC_API_KEY")
	}
	return names
}

// resolveEnvFile 决定用哪个 .env：显式指定的路径**必须存在**（显式配置静默降级
// 是事故温床），否则从 dir 起向上查找。
func resolveEnvFile(explicit, dir string) (map[string]string, string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return nil, "", err
		}
		m, err := parseEnvFile(abs)
		if err != nil {
			return nil, abs, fmt.Errorf("读取 %s 失败: %w", abs, err)
		}
		return m, abs, nil
	}
	return LoadEnv(dir)
}
