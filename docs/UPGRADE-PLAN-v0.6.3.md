# CliRelay v0.6.3 自定义补丁升级方案

状态：补丁已在本仓库移植并完成本地编译/单测。**尚未部署。生产必须保持** `clirelay-local:gpt-overflow-grok47-20261114`，直到用户明确下令发布。

源码树：`/workspace/clirelay-v0.6.3-upgrade`  
分支：`upgrade-v0.6.3`（基于官方 tag `v0.6.3`，commit `6c358782`）  
对照基线：线上二进制与 `/workspace/clirelay-ci2/_build/CLIProxyAPI` 的 sha256 一致：`493c559f9954e8584693d278e4d3d8fe2f334d07da2f563b4c469a2e5bf927bd`，自报 `CliRelay Version: v0.5.7, Commit: gpt-overflow-grok47-20261114, BuiltAt: 2026-11-14T02:26:17Z`。该树就是 11 月 14 日打进生产镜像的源码。

本地试编译（未推送到美服）：
`/workspace/clirelay-v0.6.3-upgrade/_build/CLIProxyAPI`  
自报 `CliRelay Version: v0.6.3-upgrade, Commit: upgrade-v0.6.3, BuiltAt: 2026-11-16T13:11:13Z`（约 2026-11-16 21:11 Asia/Shanghai）。

## 1. 生产自定义差异清单与移植结果

对照官方 `v0.5.7`（`0e81a38b`）与线上源码树 `clirelay-ci2`：约 82 个新增文件、78 个改动文件。这些是 10 月 9 日至 11 月 14 日叠在官方 v0.5.7 上的补丁，镜像层依次为：failure/quota/stability/overload/repair/recovery/stream-keepalive/buffer-diagnostics、OpenCode session inject、5 秒过载冷却、GPT 满并发溢出到 grok-4.7。`.env` 里仍写着旧的 `ssrf-intranet-allow` 标签，但 `docker-compose.override.yml` 和正在运行的容器都是 `gpt-overflow-grok47-20261114`；SSRF 内网放行已经编进当前二进制（`api_call.go`）。

### 已移植（必须保留的行为）

| 行为 | 现状 | v0.6.3 处理 |
| --- | --- | --- |
| A. GPT 满并发溢出到 grok-4.7 | 配置 `account-concurrency.concurrency-overflow-model: grok-4.7`。官方 v0.6.3 没有这个键。GPT 账户全满时立即改写到 grok-4.7；Grok 也满，或请求带 tools / encrypted reasoning / previous_response_id，则走原来的 GPT 队列，超时 429。键为空则关闭。 | 已按现行语义重实现，配置热加载保留。相关测试通过。 |
| B. 过载/模型级 503 冷却 | 固定 5 秒；上游 `Retry-After` 更长则遵从。额度与 401 长冷却本来就没改。 | 官方 v0.6.3 已是过载默认 5 秒，并会跟随 `Retry-After`（最短 1 秒、最长 10 分钟）。差在官方会接受短于 5 秒的 `Retry-After`。已加上 5 秒下限，只作用于过载判定；普通 5xx、额度、401 不改。 |
| C. OpenCode 会话注入 | 客户端没带 `X-Opencode-Session` 时，从其他会话头或 UUID 合成，且不写回共享凭证。官方 v0.6.3 仍只转发显式头。 | 已移植，测试通过。 |
| C. SSRF 内网放行 | 管理面板「获取 /models」可访问 Docker/内网/回环。官方 v0.6.3 仍拦截私网和 loopback。 | 已移植；仍拦截链路本地、元数据、CGNAT、组播等危险段。测试通过。 |
| D. `text-model-fallback: gpt-6-luna` | 生产正在使用。请求模型缺失、禁用或没有路由时，纯文本请求可回落到 gpt-6-luna。另支持可选 `request-model-aliases`（生产 YAML 未配置别名）。 | 已在准入（模型门控改写请求体）路径移植。配置可加载。 |
| D. 并发上限 12 / overflow 目标 / `enabled: false` | GPT-001/002/003 与 Grok 的并发上限在账户文件里，不是这段代码。`auto-update.enabled: false` 是自动更新开关。模型目录隐藏走官方 catalog/`enabled: false`。 | 不改配置语义。升级时不得删除已禁用的 model-configs 条目。 |
| 兼容键 | 生产有 `streaming.max-attempts: 4`、`streaming.recovery-timeout-seconds: 20`。 | 结构体已接收这些键，避免以后严格解析出错。行为改由官方 `bootstrap-retries`（生产显式为 1）和按通道组的 first-token timeout（生产未配置，默认关闭）承担，见下文风险。 |

### 未原样移植（有官方等价或不能干净合入）

10 月叠在 v0.5.7 上的大批诊断/恢复/传输补丁（request outcome 面板、recovery budget、buffer recovery、自定义 stream startup keepalive 实现、codex-http-transport 配置块、reasoning provenance 等）没有整文件搬进 v0.6.3。官方从 v0.5.8 到 v0.6.3 重写了调度、冷却、流式 failover（首字节前换号）、first-token timeout、Codex reasoning 密文恢复、请求日志等，强行合入会和新技术栈冲突。

其中需要明确的生产差异：

- **中途 SSE 恢复预算**（`max-attempts` / `recovery-timeout-seconds`）未在官方新流式栈上复现。官方默认是「首字节前可以换号」，首字节之后不再按旧补丁做同一套恢复窗口。
- **执行过程中的二次 text-model-fallback**（已经开始跑之后因模型/路由失败再改写）未挂进官方 `execute`/`stream`。准入阶段的缺失/禁用/无路由回落已覆盖生产配置的本意。
- **`codex-http-transport` 配置块**只在旧示例里出现，生产 YAML 没有这项；未单独移植。官方 v0.6.x 自有 Codex HTTP/缓存/推理修复。
- 自定义 request-outcome 表迁移未移植，避免和官方 v0.6 请求日志迁移打架。

## 2. 以后如何构建镜像

在本树构建，**不要**把新二进制叠到旧的 `gpt-overflow-grok47-20261114` / v0.5.7 镜像上。v0.6.3 带有新的前端/入口/依赖，应使用本树官方 `Dockerfile` 整包构建。

建议标签：`clirelay-local:v0.6.3-upgrade-YYYYMMDD`（例如 `v0.6.3-upgrade-20261116`）。

在美服构建示例（发布当日再执行，现在不要跑）：

```bash
cd /path/to/clirelay-v0.6.3-upgrade
sudo docker build \
  --build-arg VERSION=v0.6.3-upgrade \
  --build-arg COMMIT=upgrade-v0.6.3 \
  -t clirelay-local:v0.6.3-upgrade-20261116 .
```

或先在本机构出 `_build/CLIProxyAPI`，再按官方镜像打一层二进制；仍应基于 **官方 v0.6.3 镜像/本树 Dockerfile**，不要 `FROM clirelay-local:gpt-overflow-grok47-20261114`。

只重建 `cli-proxy-api`。不要动 postgres、redis、updater，也不要跑官方自动升级。`auto-update.enabled` 保持 false。

## 3. 升级步骤（等用户下令后再做）

### 3.1 备份（必须）

1. **当前镜像标签必须保留**：`clirelay-local:gpt-overflow-grok47-20261114`（以及已有的 `overload-cooldown-5s-20261112`、`opencode-session-inject-20261021`）。不要删、不要改 tag。
2. **备份 `config.yaml` 和 `docker-compose.override.yml`** 到 `/home/admin/jiezi/backups/v0.6.3-upgrade-<时间戳>/`。不要改配置语义：保留 `concurrency-overflow-model: grok-4.7`、`text-model-fallback: gpt-6-luna`、并发上限 12、已禁用的 model-configs。
3. **备份 PostgreSQL**（v0.5.7 → v0.6.3 会在启动时跑 v0.6.0/v0.6.1 的库迁移；官方说 v0.6.3 本身没有新迁移）。其中一次会**永久遮蔽请求日志里的客户端 API Key**，不可逆。务必先 `pg_dump`。迁移在启动后由集群 leader 分批进行，不阻塞 `/readyz`，但回滚代码也回不到明文 Key。
4. CORS 和 nginx 512m（cpa-cors）不属于这次代码升级，不要顺手改。

### 3.2 建议测试（先在新镜像、不要切生产流量之前）

- GPT 账户打满并发后，普通文本请求应立即落到 grok-4.7，而不是在 GPT 队列里等 30 秒。
- Grok 也满，或请求带 tools / encrypted reasoning / `previous_response_id` 时，应回到 GPT 队列，超时 429。
- 过载 503：无 `Retry-After` 时约 5 秒后可再试；更长的 `Retry-After` 被遵守；额度/401 仍是长冷却。
- 短请求各打一发 GPT（如 gpt-6-luna / gpt-6-sol）和 Grok，确认能出字。
- 对一个禁用或未配置的纯文本模型名，确认会回落到 gpt-6-luna。
- 若还在用管理面板拉内网 `/models`，确认内网地址仍可访问。
- 若有 OpenCode 客户端不带 `X-Opencode-Session`，确认不再因缺头而 400。

### 3.3 发布顺序（仅在用户明确说部署之后）

1. 备份完成并确认旧镜像标签还在。
2. 构建并 tag 新镜像，**不要**覆盖旧 tag。
3. 只改 `docker-compose.override.yml` 里 `cli-proxy-api` 的 `image:`，指向新标签。
4. `sudo docker compose --project-name clirelay --project-directory /home/admin/jiezi/clirelay up -d --no-deps --pull never --force-recreate cli-proxy-api`
5. 检查健康检查、版本字符串、overflow 日志、一次真实 GPT/Grok 请求。
6. 观察启动后的数据库迁移日志，尤其是请求日志 API Key 脱敏。

官方 v0.6.0 曾建议搭配更新 codeProxy 面板。本次镜像会带上官方新前端；不要打开自动更新。若面板功能异常，再单独评估面板版本。

### 3.4 回滚

不要删除新标签，除非以后另行决定。

```bash
# 把 override 里的 image 改回 clirelay-local:gpt-overflow-grok47-20261114
# 或恢复发布前备份的 docker-compose.override.yml
sudo docker compose --project-name clirelay --project-directory /home/admin/jiezi/clirelay \
  up -d --no-deps --pull never --force-recreate cli-proxy-api
```

回滚代码会回到旧二进制，但 **PostgreSQL 迁移和请求日志 Key 脱敏不会自动还原**。这是先备份数据库的原因。配置文件若只改了 image 行，回滚 override 即可。

## 4. 残留风险与未跑的测试

- 未在美服做集成测试，也没有用生产流量或真实 GPT/Grok 账号打过新二进制。只跑了本机单测和编译。
- 未把官方 `dev` 上尚未发版的修复打进来。截至 2026-11-16 20:33 Asia/Shanghai，`dev` 上值得留意的有：翻译器对大请求的平方级改写修复（#1301/#1303）、部署看门狗不要把慢槽位当成卡死（#1302）、按内容拒绝后冻结请求方对某供应商的访问（#1297）。基线仍是 v0.6.3。
- 中途流式恢复窗口可能变短/变样，见第 1 节。
- v0.6.0 起还有出站脱敏、敏感词、按模型配额、虚拟模型、告警、出口代理熔断等新能力，默认是否开启取决于官方默认值和现有配置；升级后观察即可，不必为这次补丁单独打开。
- 生产未改动。核查时（Asia/Shanghai）容器仍是 `clirelay-local:gpt-overflow-grok47-20261114`，自 2026-11-14 10:27 起运行且 healthy。

## 5. 生产未被改动

本次只在盒子上的 `/workspace/clirelay-v0.6.3-upgrade` 做了源码移植、编译和测试。美服只做过只读检查（`docker inspect` / 读备份说明 / 读非密钥配置项）。没有部署、重启、改配置、跑 updater，也没有给生产镜像重新打 tag。
