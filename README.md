# log-collector

独立的 YAML 驱动日志采集器。它结合了进程文件描述符发现与 OpenTelemetry 风格的文件读取/OTLP 上报，不依赖平台配置下发。

本项目基于 [Apache License 2.0](LICENSE) 开源。

## 能力

- 使用 `gopsutil/v4/process` 按进程名、命令行规则发现进程和已打开日志文件。
- 按 glob 独立采集自定义日志文件。
- 采集 Kubernetes/CRI `/var/log/containers/*.log` 标准输出，识别 CRI 时间、stdout/stderr 和 P/F 标记。
- 以 `device:inode` 保存读取位点，原子写入 position 文件；文件 truncate 后从头读取。
- 支持按时间首行规则合并多行日志，非首行内容自动追加到上一条记录。
- 自动识别常见日志级别，并写入 OTLP `severity_text`、`severity_number` 和 `log.level` 属性。
- 支持将 ERROR/FATAL 日志推送到企业微信群机器人，并可独立关闭 OTLP `/v1/logs` 输出。
- 批量构造 OTLP Logs protobuf，通过 OTLP/HTTP 上报，支持 gzip、headers、超时、字节级内存边界和指数退避重试。
- 进程快照单次复用、固定读取 worker、dirty 位点刷盘、读取大小上限和 OTLP ResourceLogs 聚合。
- 自身运行日志使用 zap，并通过 lumberjack 支持按大小滚动、历史数量、保留天数和 gzip 压缩。
- 内置全局令牌桶流控，支持阻塞反压和超限丢弃两种模式。

## 日志流控

流控基于 `golang.org/x/time/rate`，位于文件读取与 OTLP Exporter 队列之间，默认关闭：

```yaml
flow_control:
  enabled: true
  rate_per_second: 1000
  burst: 2000
  mode: block
  report_interval: 10s
```

- `block`：没有令牌时等待，将压力传递回文件读取 worker；不会主动丢弃日志。
- `drop`：没有令牌时立即丢弃，适合优先保护 CPU 和内存的场景。丢弃量按 `report_interval` 汇总到自身日志。
- `rate_per_second`：稳定速率，允许小数。
- `burst`：令牌桶容量，决定瞬时突发量。

流控只控制日志记录数量，不按日志字节数计算；单条和多行日志大小仍由 `performance` 中的大小限制负责。

## 自身日志

采集器自身日志与被采集的业务日志相互独立。默认同时写入标准输出和 `./logs/log-collector.log`：

```yaml
log:
  level: info
  format: json
  file: ./logs/log-collector.log
  also_stdout: true
  max_size_mb: 100
  max_backups: 10
  max_age_days: 7
  compress: true
```

如果只需要容器标准输出，可将 `file` 设置为空字符串并保留 `also_stdout: true`；如果只写滚动文件，则设置 `also_stdout: false`。

## 快速开始

```bash
cp config.example.yaml config.yaml
go run ./cmd/log-collector -config ./config.yaml
```

生产环境通常需要读取其他进程的 `/proc/<pid>/fd` 和日志文件。请通过用户组、文件 ACL 或容器的只读 hostPath 授权；不要无条件使用特权容器。

## 配置语义

### 进程日志

一个进程规则必须配置 `comm_regex` 或 `cmdline_regex`，两者同时配置时必须同时匹配。采集器通过 gopsutil 获取进程名、命令行和打开文件，再应用：

- `include_regex`：必填，允许采集的日志路径。
- `exclude_regex`：可选，排除路径。
- `max_files`：每个匹配进程的最大日志数；`0` 表示不限制。

在 Linux 上，gopsutil 底层仍以 procfs 作为内核进程信息来源，但本项目不再自行遍历和解析 `/proc`。

### 自定义文件

`sources.files` 使用 Go `filepath.Glob` 规则。它支持 `*`、`?` 和字符范围，不支持 `**` 递归语义。

### 容器标准输出

`sources.containers` 同样使用 glob。Kubernetes 推荐挂载宿主机 `/var/log/containers` 并配置：

```yaml
include: [/var/log/containers/*.log]
format: cri
```

文件名符合 `<pod>_<namespace>_<container>-<id>.log` 时，会自动生成 `k8s.pod.name`、`k8s.namespace.name`、`k8s.container.name` 和 `container.id`。

CRI 的 `P`（partial）记录会保留为独立的 OTLP LogRecord，并通过 `log.cri.flag=P` 标识，不会在采集器内跨行拼接；如需合并，建议由下游按 CRI 标记处理。

### start_at

- `end`：没有历史位点的新文件从文件末尾开始。
- `beginning`：没有历史位点的新文件从头开始。

已有 inode 位点时，始终从保存的位置继续。若保存位置大于当前文件大小，则视为 truncate 并从头读取。

### 多行

多行合并只配置 `start_pattern`：匹配的行表示一条新日志开始，所有不匹配的行都会追加到上一条日志。下面的规则同时支持纯时间、年月日、ISO `T`、斜杠日期、点号或逗号小数秒以及时区偏移：

```yaml
multiline:
  start_pattern: '^(?:(?:\d{4}[-/]\d{2}[-/]\d{2})[ T])?\d{2}:\d{2}:\d{2}(?:[.,]\d{3,9})?(?:Z|[+-]\d{2}:?\d{2})?'
  flush_after: 3s
```

未遇到下一条记录时，`flush_after` 到期会输出缓冲内容，默认值为 5 秒。

### 日志属性抽取

进程、文件和容器规则都可以配置 `attribute_extractors`。每个抽取器需要指定 OTLP LogRecord 属性名和正则表达式，正则的第一个捕获组作为属性值；没有匹配时不会生成该属性：

```yaml
attribute_extractors:
  - key: request.id
    pattern: '\b(?:TRACE|DEBUG|INFO|WARN|ERROR|FATAL)\s+-\s+(\S+)\s+-'
```

例如日志片段 `DEBUG - qzJFsZlazjUljpmV -` 会生成 `request.id=qzJFsZlazjUljpmV`。抽取在多行合并完成后执行，因此正则可以匹配完整日志正文。

### 按日志级别丢弃

每个进程、文件或容器规则都可以配置不发送的日志级别：

```yaml
drop_levels:
  - TRACE
  - DEBUG
```

支持 `TRACE`、`DEBUG`、`INFO`、`WARN`、`ERROR` 和 `FATAL`，配置值不区分大小写。日志会先完成多行合并和级别标准化；命中 `drop_levels` 后仍会正常推进文件读取位点，但不会进入流控器、Exporter 队列或 OTLP 请求。未识别出级别的日志不会被该配置丢弃。

## OTLP 输出

采集器会从每条日志的首行识别 `TRACE`、`DEBUG`、`INFO`、`NOTICE`、`WARN/WARNING`、`ERROR/ERR`、`FATAL/CRITICAL/CRIT/ALERT/EMERG/PANIC`。级别会归一化为 `TRACE`、`DEBUG`、`INFO`、`WARN`、`ERROR` 或 `FATAL`，并同时写入 OTLP 标准严重性字段和 `log.level` 属性；未识别到级别时保持 OTLP 严重性未指定。多行日志仅以首行为准。

`export.endpoint` 可填写 `http://host:4318`，程序自动补充 `/v1/logs`；也可以填写完整地址。请求格式为：

```text
POST /v1/logs
Content-Type: application/x-protobuf
Content-Encoding: gzip  # compression: gzip 时
```

发送失败时当前批次会保留并指数退避重试；超过 `max_elapsed_time` 后进程返回错误，由进程管理器负责重启。

批处理和内存队列同时按照“记录数”和“估算字节数”限制。建议生产环境显式配置：

```yaml
export:
  batch_size: 500
  max_batch_bytes: 4194304
  queue_size: 10000
  max_queue_bytes: 67108864
```

`max_queue_bytes` 必须大于或等于 `max_batch_bytes`。单条记录超过队列字节上限时会返回错误，不会无限占用内存。HTTP 408、429、5xx 和网络错误会重试；其他 4xx 配置类错误直接返回。

如需关闭 OTLP 输出：

```yaml
export:
  enabled: false
```

关闭后不会创建 OTLP 队列和发送协程，也不会访问 `/v1/logs`。

## 企业微信错误日志推送

```yaml
wechat_webhook:
  enabled: true
  url: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me'
  title: 日志异常告警
  ignore_keywords:
    - known harmless error
    - connection reset by peer
  error_type_keywords:
    - AuthenticationException
    - DatabaseException
  timeout: 5s
  max_content_length: 4000
```

只有解析为 `ERROR` 或 `FATAL` 的日志会推送。若合并后的完整正文包含 `ignore_keywords` 中任一关键词，则跳过推送；匹配使用 `strings.Contains`，区分大小写，空值和重复值会被配置校验拒绝。将采集器自身的 `log.level` 设置为 `debug` 后，会输出被忽略日志的命中关键词、级别、来源、文件、请求 ID 和完整正文。

`error_type_keywords` 用于给通知分类，不会过滤通知。正文命中列表中的关键词时，按配置顺序取第一个匹配项，在企业微信通知中增加“错误类型”字段；没有命中时不显示该字段。匹配同样区分大小写。

消息使用 Markdown 告警样式，包含主机、时间、日志级别、来源规则、文件路径、可选的 `request.id` 和多行合并后的正文。正文按原始换行逐行输出，不插入 `<br>`，并转义反引号以避免 SQL 字段被渲染成行内代码。主机优先读取 `host.name` Resource 属性，否则使用采集器所在主机名。Webhook 地址包含机器人密钥，不会被程序写入错误日志；生产环境应通过部署系统注入并限制配置文件权限。

OTLP 和企业微信可以同时开启，也可以只开启其中一个；两者同时关闭会导致启动配置校验失败。两者同时开启时，Webhook 暂时失败不会阻断 OTLP 输出；仅启用 Webhook 时，推送失败会返回错误，避免日志在没有任何成功输出的情况下继续推进位点。

## 配置热加载

采集器使用 `fsnotify` 监听配置文件所在目录，并对文件保存事件进行 300ms 防抖。修改配置文件后无需重启即可生效的配置包括：

- `log.level`
- `wechat_webhook` 下的全部配置，包括开关、地址、标题、超时、内容长度、忽略关键词和错误类型关键词

| 配置项 | 是否热加载 | 生效方式 |
| --- | --- | --- |
| `log.level` | 是 | 通过 zap `AtomicLevel` 原子更新 |
| `wechat_webhook.enabled` | 是 | 启用或关闭后续 Webhook 推送 |
| `wechat_webhook.url` | 是 | 新建 Webhook Client 后原子替换 |
| `wechat_webhook.title` | 是 | 后续通知使用新标题 |
| `wechat_webhook.timeout` | 是 | 新建 HTTP Client 后生效 |
| `wechat_webhook.max_content_length` | 是 | 后续通知使用新的内容限制 |
| `wechat_webhook.ignore_keywords` | 是 | 后续日志使用新忽略列表 |
| `wechat_webhook.error_type_keywords` | 是 | 后续通知使用新错误类型列表 |
| `log` 中除 `level` 外的配置 | 否 | 修改后需要重启 |
| `state`、`sources`、`export` | 否 | 修改后需要重启 |
| `performance`、`flow_control` | 否 | 修改后需要重启 |

配置文件内也使用 `[支持热加载]` 和 `[整个区块支持热加载]` 注释标记了对应配置。

热加载时先重新读取并完整校验 YAML，再构造新的 Webhook Client，最后通过原子指针整体替换。配置非法或新 Client 创建失败时继续使用旧配置。Webhook URL 不会输出到热加载日志。

以下配置发生变化时会输出 `configuration changes require restart` 警告，但不会修改当前运行状态：

- `log` 中除 `level` 外的文件、格式和滚动参数
- `state`
- `sources`
- `export`
- `performance`
- `flow_control`

热加载成功时，自身日志会输出 `configuration hot reload applied` 及当前日志级别、Webhook 开关和关键词数量。

## 验证命令

```bash
go mod tidy
go test ./...
go vet ./...
go build ./cmd/log-collector
```

这些命令交付时未自动执行。

## Makefile

项目根目录提供统一命令入口：

```bash
make help             # 查看全部命令
make fmt              # 格式化源码
make tidy             # 整理依赖
make test             # 运行测试
make vet              # 静态检查
make build            # 构建当前平台二进制
make build-linux      # 构建 Linux AMD64 和 ARM64
make build-linux-amd64 # 仅构建 Linux AMD64
make build-linux-arm64 # 仅构建 Linux ARM64
make docker-build     # 构建容器镜像
make docker-build-multi # 构建并推送 AMD64/ARM64 多平台镜像
make run              # 使用 config.yaml 启动
make clean            # 删除 dist 目录
```

## Docker 镜像

项目使用多阶段构建和 distroless 运行镜像，最终镜像只包含静态链接的可执行文件与 Apache-2.0 协议文件：

```bash
make docker-build DOCKER_IMAGE=ghcr.io/ha1377311454/log-collector:v0.1.0
docker push ghcr.io/ha1377311454/log-collector:v0.1.0
```

使用 Docker Buildx 交叉构建并推送包含 `linux/amd64`、`linux/arm64` 的多平台镜像：

```bash
docker buildx create --name log-collector-builder --use  # 首次执行一次
make docker-build-multi \
  DOCKER_IMAGE=ghcr.io/ha1377311454/log-collector:v0.2.0
```

`docker-build-multi` 使用 `--push` 直接推送 OCI Manifest，因为 Docker 本地镜像存储不能通过 `--load` 同时载入多个平台。可通过 `DOCKER_PLATFORMS` 覆盖目标平台：

```bash
make docker-build-multi \
  DOCKER_IMAGE=registry.example.com/log-collector:v0.2.0 \
  DOCKER_PLATFORMS=linux/amd64,linux/arm64
```

推送版本 Tag 后，GitHub Release 工作流还会自动将同一份多平台镜像发布为：

```text
ghcr.io/ha1377311454/log-collector:v0.2.0
ghcr.io/ha1377311454/log-collector:latest
```

可以检查远程 Manifest 中的平台：

```bash
docker buildx imagetools inspect ghcr.io/ha1377311454/log-collector:v0.2.0
```

本地运行时需要挂载配置和位点目录：

```bash
docker run --rm \
  -v "$PWD/config.yaml:/etc/log-collector/config.yaml:ro" \
  -v "$PWD/data:/var/lib/log-collector" \
  -v /var/log:/var/log:ro \
  log-collector:latest
```

镜像默认以非 root 用户运行。若宿主机日志权限不允许该用户读取，应优先通过文件用户组或 ACL 授权；不要直接使用特权容器。

## Kubernetes 部署

`deploy/kubernetes/log-collector.yaml` 提供 DaemonSet 部署示例，每个节点运行一个采集器，并完成以下挂载：

- 宿主机 `/var/log` 只读挂载，用于读取 Kubernetes CRI 容器日志。
- 宿主机 `/var/lib/log-collector` 读写挂载，用于按节点持久化读取位点。
- ConfigMap 挂载到 `/etc/log-collector`，提供采集配置。

DaemonSet 需要读取节点日志并写入由 `hostPath` 创建的位点目录，因此示例以 UID 0 运行，但关闭权限提升、删除全部 Linux capabilities、使用只读根文件系统，并且没有启用 `privileged`。

部署前修改清单中的镜像版本和 `export.endpoint`，然后执行：

```bash
kubectl apply -f deploy/kubernetes/log-collector.yaml
kubectl -n observability rollout status daemonset/log-collector
kubectl -n observability logs -l app.kubernetes.io/name=log-collector --tail=100
```

更新 ConfigMap 后，Kubernetes 使用符号链接切换挂载内容，当前配置监听器不会将该事件识别为普通文件写入。请显式滚动重启使配置生效：

```bash
kubectl -n observability rollout restart daemonset/log-collector
```

如需采集宿主机上的其他文件目录，可按 `/var/log` 的方式增加 `hostPath` 和 `volumeMount`，并在 `sources.files` 中配置容器内的对应绝对路径。

## GoReleaser

`.goreleaser.yaml` 默认生成 Linux AMD64 和 ARM64 的 `tar.gz` 制品，并携带 README、示例配置及 `checksums.txt`。

```bash
make release-check     # 检查 GoReleaser 配置
make release-snapshot  # 本地快照打包，不发布
make release-local     # 正式格式打包，但跳过远程发布
```

### GitHub 自动发布

代码合并到 GitHub 后，创建并推送以 `v` 开头的版本 Tag，即可触发 `.github/workflows/release.yml`：

```bash
git tag -a v0.1.0 -m "release v0.1.0"
git push origin v0.1.0
```

工作流会依次执行依赖校验、单元测试和静态检查，然后通过 GoReleaser 构建以下制品并上传到对应的 GitHub Release：

- `log-collector_<版本>_linux_amd64.tar.gz`
- `log-collector_<版本>_linux_arm64.tar.gz`
- `checksums.txt`

也可以在 GitHub 仓库的 `Actions -> Release -> Run workflow` 中手动执行，输入一个已经存在的版本 Tag。工作流使用 GitHub 自动提供的 `GITHUB_TOKEN`，不需要额外配置 Personal Access Token。

如需在本地发布，仍可自行设置 `GITHUB_TOKEN` 后执行：

```bash
goreleaser release --clean
```

## 联系方式

如有问题或建议，请发送邮件至 [1617802907@qq.com](mailto:1617802907@qq.com)。
