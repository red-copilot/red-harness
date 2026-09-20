package executor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 本文件是 PLAN.md:52 的「provider 流量通过宿主侧域名白名单代理」的落地。
//
// # 为什么代理是**必需**的，不是可选优化
//
// 容器跑在 `--internal` 网络里（没有默认路由），因此**没有 DNS**：它解析不了
// 任何域名。而 provider 的 API（`api.example.com` 之类）只认域名。所以 provider
// 流量只有一条路——容器把请求交给宿主侧的代理，由宿主解析域名、按白名单放行、
// 代容器建连。容器需要的网络能力因此收缩到「能到网桥网关的那一个端口」。
//
// # 为什么只做 CONNECT、不做 MITM
//
// provider 走 HTTPS，客户端先发 CONNECT 建隧道。这里只做「域名 + 端口」的准入
// 判定，**不解析 TLS**。理由是 MITM 需要代理持有证书并解密流量，那会把 provider
// 的凭据带进代理进程，也让代理本身成为一个新的高价值攻击面；而我们的目标只是
// 「哪些域名可以出站」，域名在 CONNECT 行里就看得见。
//
// # 失败模式与对策
//
//   - 白名单为空：全拒（fail closed）。空名单若退化成全放行，「provider 白名单」
//     就成了一句空话——而这类退化不会有人发现，因为一切「看起来正常」。
//   - 非 443 端口：全拒。CONNECT 允许任意端口会让代理变成通用 TCP 转发器，
//     容器里的 agent 可以拿它当跳板去连白名单域名上的任意服务。
//   - 裸 IP：全拒。白名单是**域名**白名单，放行 IP 等于绕开域名约束。
//   - 拒绝时返回明确的 403：不返回状态码而直接断连，客户端只会看到「连接被重置」，
//     无法区分「代理拒绝」与「代理挂了」——前者是策略，后者是故障。

// providerProxy 是宿主侧的 provider 域名白名单代理。
type providerProxy struct {
	allow *allowlistProxy
	ln    net.Listener
	srv   *http.Server
	once  sync.Once
	// logf 用于诊断。**绝不能记 URL 路径或 body**——provider 请求里可能带凭据。
	logf func(format string, args ...any)
}

// startProviderProxy 在 bind 上起一个 CONNECT 代理，返回可直接 Close 的实例。
//
// 监听地址必须是容器能到的宿主地址（本 run 的网桥网关），不能是 127.0.0.1：
// 容器里的 loopback 是它自己的，指向宿主 loopback 的地址根本不存在。
func startProviderProxy(bind string, cfg proxyConfig, logf func(string, ...any)) (*providerProxy, error) {
	allow, err := newAllowlistProxy(cfg)
	if err != nil {
		return nil, harness.Ef(harness.KindConfig, "executor.proxy", "provider 白名单非法", err)
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, harness.Ef(harness.KindExecutor, "executor.proxy",
			fmt.Sprintf("provider 代理无法监听 %s", bind), err)
	}
	p := &providerProxy{allow: allow, ln: ln, logf: logf}
	p.srv = &http.Server{
		Handler: p,
		// 代理不读 body（CONNECT 没有 body），但要把请求头限制住，
		// 避免一个畸形请求把内存吃光。
		MaxHeaderBytes:    16 << 10,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		// ErrServerClosed 是正常关闭路径，不算故障。
		if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log("provider 代理退出: %v", err)
		}
	}()
	return p, nil
}

// Close 关闭代理。幂等：Reclaim 与 Prepare 的失败路径都会调它。
func (p *providerProxy) Close() error {
	if p == nil {
		return nil
	}
	var err error
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err = p.srv.Shutdown(ctx)
		_ = p.ln.Close()
	})
	return err
}

// Addr 返回实际监听地址（bind 传 :0 时用于测试取随机端口）。
func (p *providerProxy) Addr() string {
	if p == nil || p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

func (p *providerProxy) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// ServeHTTP 实现 CONNECT 代理。
func (p *providerProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// 只支持 CONNECT。普通代理请求（`GET http://host/path`）会让容器把
		// 代理当通用 HTTP 转发器用，而白名单判定只发生在 CONNECT 上。
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}
	if !p.allow.allows(r.Host) {
		p.log("代理拒绝（不在 provider 白名单）: %s", r.Host)
		http.Error(w, "host not in provider allowlist", http.StatusForbidden)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	// 回 200 之前先建上游连接：上游失败时返回 502，客户端能区分
	// 「域名不在白名单（403）」与「上游连不上（502）」。
	upstream, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		p.log("代理上游连接失败: %s: %v", r.Host, err)
		_, _ = client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Hijack 之后 buf 里可能已有客户端在 200 之前就发来的字节（TLS ClientHello），
	// 必须先冲出去，否则握手会卡住——这是 CONNECT 代理最经典的挂起原因。
	if n := buf.Reader.Buffered(); n > 0 {
		head, _ := buf.Reader.Peek(n)
		if _, err := upstream.Write(head); err != nil {
			return
		}
		_, _ = buf.Reader.Discard(n)
	}

	// 双向转发。任一方向结束就关掉两端：CONNECT 隧道是「一体」的，
	// 半关会让另一端永久等待（表现为 pi 的请求永不返回）。
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
