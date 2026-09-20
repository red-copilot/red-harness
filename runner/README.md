# runner 镜像

`red-harness-runner:v0.3.0` —— 在**被约束的一侧**提供 pi 与安全工具链。它跑在
`executor/docker.go` 渲染出来的容器里；容器的隔离属性（只读 rootfs、`cap-drop=ALL`、
非 root、资源上限、`--internal` 网络）由 **argv** 决定，不由这个镜像决定。镜像只
保证「在那种环境下 pi 能起来」。

## 构建

```bash
docker build -t red-harness-runner:v0.3.0 runner/
```

## 本轮构建结果：**成功**（本机真构建，非延后）

| 项 | 值 |
|---|---|
| 镜像 ID | `sha256:4e08f9133cd2f36d30fe26011d3a4308d6f7eef5ebbcdc203867608c37b66e98` |
| 基础镜像 | `ghost/kali:latest` |
| 基础镜像 ID | `sha256:b812be39d00e4aeff12aa1d8696f44b7afd6f2098da1c69b4730f37483202c1c` |
| 体积 | 1.93 GB（519 MB 压缩内容） |
| pi | 0.85.1 |
| node | v22.23.2 |

**基础镜像 digest 是必录项**：`:latest` 会漂移，而「runner 里的 pi 是哪个版本」
是排查 agent 行为差异的第一手信息——前身被 pi 0.74.2 静默烧题库咬过，而当时
「镜像是谁构建的、里面是什么版本」没有留下任何痕迹。

## 为什么基础镜像是本机已有的 `ghost/kali:latest`

它已经装好了需要的全部东西（node v22.23.2、`@earendil-works/pi-coding-agent`、
nmap/sqlmap/hydra/ffuf/gobuster/socat/ncat/openvpn/python3）。从零装这些要下载
1.6 GB 以上，而本机只有约 9 GB 空闲磁盘。

## 为什么 pi 在镜像里自带一份，不从宿主拷

宿主上的 pi 在 `/root/.local/share/pi-node/...`，那是**宿主**的资产。把宿主的 pi
目录拷进镜像等于让容器依赖宿主的安装布局，而隔离模型里宿主状态目录是明确不许
进容器的。镜像自带一份，容器与宿主完全解耦。

（已实测确认：镜像内**不存在** `/root/.local/share/pi-node`，`command -v pi` 指向
`/usr/local/bin/pi`。）

## 镜像自检

构建时把工具清单写进 `/opt/red-harness/RUNNER_SELFCHECK.txt`（**刻意不在自检里
失败**：缺一个工具不该让镜像构建不出来，把结果写进文件由 `doctor` 与集成测试去
断言）。本轮实际内容：

```
runner=red-harness-runner:v0.3.0
base=ghost/kali:latest
OK   pi / node / npm / python3
OK   nmap / sqlmap / hydra / ffuf / gobuster / socat / nc / ncat
OK   openvpn / tcpdump / john / pkill / timeout
pi_version=0.85.1
node_version=v22.23.2
```

17/17 全部 OK。

## 默认命令是长睡

容器的生命周期由 executor 管（`Prepare` 起容器、`Exec` 往里发命令、`Reclaim`
删容器）。容器自己退出会让 `Exec` 全部失败，而失败原因看起来是「容器不见了」
——离真正的原因（镜像的 CMD 干完活退了）很远。所以 CMD 是一个不会自己结束的
东西，且用 exec 形式，让信号能正确送达。

## 集成测试

```bash
go test -tags integration ./executor/... -count=1
```

需要本机有 Docker **且镜像已构建**。覆盖：只读文件系统写入被拒、资源限制生效、
目标端点可达、非授权端点不可达、容器内 `pi --version` 可用、`Reclaim` 后容器与
网络消失。
