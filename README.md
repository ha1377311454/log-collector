# log-collector

独立的 YAML 驱动日志采集器。它结合了进程文件描述符发现与 OpenTelemetry 风格的文件读取/OTLP 上报，不依赖平台配置下发。

## 能力

- 使用 `gopsutil/v4/process` 按进程名、命令行规则发现进程和已打开日志文件。
- 按 glob 独立采集自定义日志文件。
- 采集 Kubernetes/CRI `/var/log/containers/*.log` 标准输出，识别 CRI 时间、stdout/stderr 和 P/F 标记。
- 以 `device:inode` 保存读取位点，原子写入 position 文件；文件 truncate 后从头读取。
- 支持“新记录起始行”或“上一条记录续行”两种多行模式。
- 批量构造 OTLP Logs protobuf，通过 OTLP/HTTP 上报，支持 gzip、headers、超时和指数退避重试。
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

同一规则只能配置一种：

- `start_pattern`：匹配行表示一条新日志开始。
- `continuation_pattern`：匹配行追加到上一条日志；不匹配行开始新日志。

未遇到下一条记录时，`flush_after` 到期会输出缓冲内容，默认值为 5 秒。

## OTLP 输出

`export.endpoint` 可填写 `http://host:4318`，程序自动补充 `/v1/logs`；也可以填写完整地址。请求格式为：

```text
POST /v1/logs
Content-Type: application/x-protobuf
Content-Encoding: gzip  # compression: gzip 时
```

发送失败时当前批次会保留并指数退避重试；超过 `max_elapsed_time` 后进程返回错误，由进程管理器负责重启。

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
make run              # 使用 config.yaml 启动
make clean            # 删除 dist 目录
```

## GoReleaser

`.goreleaser.yaml` 默认生成 Linux AMD64 和 ARM64 的 `tar.gz` 制品，并携带 README、示例配置及 `checksums.txt`。

```bash
make release-check     # 检查 GoReleaser 配置
make release-snapshot  # 本地快照打包，不发布
make release-local     # 正式格式打包，但跳过远程发布
```

创建并推送语义化版本 tag 后，可执行正式发布：

```bash
export GITHUB_TOKEN="<你的Token>"
git tag -a v0.1.0 -m "release v0.1.0"
git push origin v0.1.0
goreleaser release --clean
```
