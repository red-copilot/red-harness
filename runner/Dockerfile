# red-harness runner 镜像 v0.3.0
#
# 这个镜像的职责：在**被约束**的一侧提供 pi 与安全工具链。它跑在
# executor/docker.go 渲染出来的容器里，容器的隔离属性（只读 rootfs、
# cap-drop=ALL、非 root、资源上限、--internal 网络）由 argv 决定，不由这个
# Dockerfile 决定——Dockerfile 只需要保证「在那种环境下 pi 能起来」。
#
# # 基础镜像为什么是 ghost/kali:latest
#
# 本机已有的 `ghost/kali:latest`（Kali GNU/Linux Rolling 2026.3）已经装好了
# 我们需要的全部东西：node v22.23.2、`@earendil-works/pi-coding-agent`（pi
# 0.85.1）、nmap/sqlmap/hydra/ffuf/gobuster/socat/ncat/openvpn/python3。
# 从零装这些要下载 1.6 GB 以上，而宿主只有 9.5 GB 空闲磁盘。基础镜像的 digest
# 记录在 README.md 里——**digest 是必录项**：`:latest` 会漂移，而「runner 里
# 的 pi 是哪个版本」是排查 agent 行为差异的第一手信息。
#
# # 为什么保留 pi 在 /usr/local（而不是从宿主拷一份）
#
# 宿主上的 pi 在 `/root/.local/share/pi-node/...`，那是**宿主**的资产；把宿主
# 的 pi 目录拷进镜像等于让容器依赖宿主的安装布局，而隔离模型里宿主状态目录是
# 明确不许进容器的。镜像自带一份，容器与宿主就完全解耦了。
#
# # 构建
#
#   docker build -t red-harness-runner:v0.3.0 runner/
#
# 构建必须是**可复现**的：不装任何需要联网的额外包（apt 只用来补几个缺失的
# 工具，且用 --no-install-recommends + 清 lists 控制体积）。

FROM ghost/kali:latest

# 非交互：apt 在容器里没有 tty，不设这个会在某次 debconf 提问上永久挂住
# （表现为「构建卡住、没有任何输出」，最容易被误判成网络慢）。
ENV DEBIAN_FRONTEND=noninteractive
# 容器内的工作目录与 HOME 由 executor 的 argv 决定（HOME 落在工作目录下的
# .home），这里只提供缺省值，保证「手动 docker run 进来看一眼」也能用。
ENV LANG=C.UTF-8

# ── 补齐缺的安全工具 ──
#
# 基础镜像已有 nmap/sqlmap/hydra/ffuf/gobuster/socat/python3/openvpn，
# 这里只补三样在渗透流程里常用、而基础镜像没有的：
#   - netcat-openbsd：`nc` 的完整实现（基础镜像只有 ncat，行为有差异）
#   - john：口令破解
#   - tcpdump：抓包（基础镜像已有，显式列出以防基础镜像换版本时丢）
#
# 用 `apt-get update && install && rm -rf /var/lib/apt/lists/*` 一条 RUN 完成：
# 分成多条会让 apt lists 留在镜像层里（几十 MB）。
# 用 `|| true` 包住：这几个包缺失**不应该**让整个镜像构建失败——核心能力
# （pi + node + nmap 系）来自基础镜像，补包失败只影响几个边缘工具。
RUN set -eux; \
    apt-get update -o Acquire::Retries=3; \
    apt-get install -y --no-install-recommends \
        netcat-openbsd \
        john \
        tcpdump \
        iproute2 \
        procps \
        ca-certificates \
    ; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# ── 工作目录与 HOME ──
#
# /work 是 executor 默认挂载点的容器侧（ExecutorSpec.Workdir 缺省 "/work"）。
# 它必须是**可写**的：只读 rootfs 下唯一的可写区是 --tmpfs /tmp 与这个挂载点，
# 而 pi 的会话目录、扩展缓存都要写在 HOME 里，所以 executor 把 HOME 指到
# /work/.home。
#
# 权限 0777：容器以 `--user 65534:65534`（nobody）运行，而挂载进来的宿主目录
# 属主是宿主用户。0777 保证无论宿主的 uid 是什么，容器内的 nobody 都能写。
# 这不是放宽隔离——这个目录本来就是 agent 唯一被允许写的宿主位置，
# 真正被禁止的是「写到工作目录之外」（那由只读 rootfs 拦住）。
RUN mkdir -p /work /work/.home && chmod 0777 /work /work/.home

# ── 自检 ──
#
# 构建期就把「镜像里到底有什么」写进一个文件，而不是等到运行期才发现 pi 不在。
# 这是对前身事故的直接回应：pi 版本漂移（0.74.2 静默烧题库）之所以难查，就是
# 因为「镜像是谁构建的、里面是什么版本」没有留下任何痕迹。
#
# 刻意**不**在自检里失败：缺一个工具不该让镜像构建不出来；把结果写在文件里，
# 由 `doctor` 与集成测试去断言。
RUN set -eu; \
    log=/opt/red-harness/RUNNER_SELFCHECK.txt; \
    mkdir -p /opt/red-harness; \
    : > "$log"; \
    echo "runner=red-harness-runner:v0.3.0" >> "$log"; \
    echo "base=ghost/kali:latest" >> "$log"; \
    for b in pi node npm python3 nmap sqlmap hydra ffuf gobuster socat nc ncat openvpn tcpdump john pkill timeout; do \
        if command -v "$b" >/dev/null 2>&1; then \
            echo "OK   $b" >> "$log"; \
        else \
            echo "MISS $b" >> "$log"; \
        fi; \
    done; \
    echo "pi_version=$(pi --version 2>/dev/null || echo unknown)" >> "$log"; \
    echo "node_version=$(node --version 2>/dev/null || echo unknown)" >> "$log"; \
    cat "$log"

# 默认命令：**长睡**。
#
# 为什么不是某个有用的命令：容器的生命周期由 executor 管（Prepare 起容器、
# Exec 往里发命令、Reclaim 删容器）。容器自己退出会让 Exec 全部失败，
# 而失败原因看起来是「容器不见了」——离真正的原因（镜像的 CMD 干完活退了）
# 很远。所以 CMD 必须是一个不会自己结束的东西。
#
# 为什么不是 `sleep infinity` 加 exec 形式：这里是 CMD 的 exec 形式，
# sleep 就是 PID 1 的直系子进程（docker-init 是 PID 1），信号能被正确送达。
CMD ["sleep", "infinity"]

# 注意：**不声明 USER**。
#
# 容器内的用户由 executor 的 `docker run --user 65534:65534` 与
# `docker exec --user 65534:65534` 决定。在镜像里写死 USER 会给人一种
# 「隔离已经由镜像保证了」的错觉——而镜像里的 USER 完全可以被
# `docker run --user 0` 覆盖。隔离必须落在 argv 上（见 executor/spec.go），
# 镜像层不承担这个责任。
#
# 同理，这里**不**写 `VOLUME /work`：VOLUME 会让 Docker 自动创建一个匿名卷，
# 而我们要的是显式 `-v <宿主目录>:/work`。两者同时存在时，匿名卷会挡住挂载点
# 的内容，表现为「工作目录里什么都没有」。
