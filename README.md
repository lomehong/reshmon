# reshmon — Windows 资源健康监护小工具

多 AI Agent 并发干活时，回答四个问题：**是谁、在什么时候、把哪个资源（CPU/内存/磁盘 IO/GPU）、吃掉了多少**。定位是诊断工具：只"看见"和"留证"，不自动限流、不杀进程、不发通知（机器真死了通知也发不出去——活着的机器留证据，事后可回溯）。

- 覆盖 CPU（总体+每核+每进程）、内存（物理+提交）、磁盘 IO（吞吐/忙碌/延迟/队列，按物理盘）、磁盘剩余空间、GPU（利用率/显存，按进程归因，NVIDIA/AMD/Intel 全厂商）、网络（展示用）
- **CPU 温度与功耗**：ACPI 热区温度（封装附近，非核心温度）+ Windows `Energy Meter` 的封装/核心/核显功率，标准权限即可读；机器没有就不显示、不算采集降级
- **端口 → 进程归属**：全部 TCP/UDP 端点及其归属进程与 Agent，回答"谁占着这个端口"
- **点击趋势图回溯**：点曲线上任意时刻，直接列出**当时**最忙的进程与 Agent 账本（覆盖最近 2 小时）；落在数据缺失区间会如实标注盲区
- **Agent 归因**：按进程树聚合——"Agent 及其全部子进程（编译器/测试/node…）"算成一笔账；**嵌套 Agent 各记各的**（ZCode 里跑 Claude：Claude 及其子树归 Claude，ZCode 自身与其余子树归 ZCode）；内置 ZCode/Claude/Codex/Cursor/Qoder/Ollama 等识别规则，可自行扩展。归因带 PID 复用防护（PID+进程创建时间双因子），换壳进程不会记到前任头上
- **短命进程 IO 归因（ETW）**：管理员运行时订阅内核 DiskIO+Process 事件（IRP 配对），存活不足一个采样间隔的进程（编译/链接/测试的一次性 IO）也进 Agent 账本；非管理员自动降级，不影响其余功能
- **MCP 接口**：`reshmon mcp`（stdio）或面板同端口的 `POST /mcp` 暴露资源快照/Agent 账本/趋势/事件 5 个工具——AI Agent 发起任务前自查机器负载，自行决策并发与时机
- **历史落盘**：每 2s 总量+Agent 账本、每 30s 进程切片，按天轮转 gzip，默认保留 14 天
- 零第三方运行时依赖（仅 golang.org/x/sys），GPU 走 Windows 性能计数器（厂商无关），NVIDIA 温度经 nvml.dll 机会性增强

## 构建

```bash
cd D:\Develop\Projects\reshmon
go build -o bin/reshmon.exe ./cmd/reshmon
```

要求：Windows 10 1709+（GPU 计数器）、Go 1.27+。普通权限即可运行；以管理员运行可看到更多受保护进程的内存/IO 明细，并启用 ETW 短命进程 IO 归因。

## 五个命令

### 1. once —— 单次体检（脚本/顺手看一眼）

```bash
D:\Develop\Projects\reshmon\bin\reshmon.exe once
```

打印 CPU/内存/磁盘/GPU/网络快照 + Agent 排名。**退出码：0=健康，1=亚健康，2=饱和，3=执行错误**（`-json` 输出同样适用；采集降级时至少退出 1，不让"看不见"伪装成"健康"），可直接接进调度脚本：

```bash
reshmon.exe once && echo OK
reshmon.exe once -json          # 机器可读 JSON（完整进程表+健康结论）
reshmon.exe once -obs 1500ms    # 加大观察窗，速率更稳（默认 800ms）
```

### 2. serve —— Web 面板 + 常驻记录（推荐日常挂着）

```bash
D:\Develop\Projects\reshmon\bin\reshmon.exe serve
```

打开面板：**http://127.0.0.1:8964/**

- 顶部健康卡片（CPU/内存/提交/每块磁盘/每块 GPU，红黄绿三档 + 迷你趋势）。有读数时 CPU 卡片附带温度/功耗
- 趋势图（5 分钟/30 分钟/2 小时，可切 CPU/内存/提交/温度/功耗/磁盘忙/GPU/网络）
- **点击趋势图任意时刻回溯"当时是谁在吃资源"**：列出该时刻的进程 Top（按当前所选指标排序）与 Agent 账本。进程概览约每 6 秒一份、覆盖最近 2 小时（与趋势图最长档一致），内存占用固定；点曲线两侧留白或切到更短的时间档可取消。该时刻若落在数据缺失区间（记录器冻结/睡眠）会**如实标注盲区**而非返回空列表
- Agent 桶表（CPU/内存/IO/GPU/显存/进程数）、进程表（点击列头按任意维度排序）
- **端口 → 进程**表：全部 TCP/UDP 端点及其归属进程与 Agent，监听在前、按端口数值升序，可按端口/进程/Agent 过滤（瞬时状态，不落盘；一次最多显示 300 条，用过滤框缩小范围）
- 健康事件时间线（状态迁移全记录，读最近 7 天）

接口：`/api/snapshot`、`/api/history?sec=`、`/api/procs`、`/api/conns?port=&pid=`、`/api/at?t=<unix毫秒>`、`/api/incidents?n=`，以及 MCP 的 `POST /mcp`。

参数：`-addr 127.0.0.1:8964`（改端口）、`-data 目录`（改数据目录）、`-no-record`（只看不落盘）、`-interval 1s`。

安全口径：默认只绑回环地址，且 API 校验 Host 头（防 DNS rebinding 借浏览器读走进程表）。如需手机/局域网访问用 `-addr 0.0.0.0:8964`——此时**面板对局域网无鉴权全量可见**（含全部进程路径），只在自己信任的网络里这么开。

### 3. live —— 终端实时面板（SSH/无浏览器场景）

```bash
D:\Develop\Projects\reshmon\bin\reshmon.exe live
```

Ctrl+C 退出；默认同样记录历史。参数：`-data 目录`、`-no-record`、`-interval 1s`。

### 4. mcp —— MCP 服务器（让 Agent 自查资源后决策并发）

两种接入方式：

**A. Streamable HTTP（推荐，serve 已在跑时）**——面板同端口直接提供 MCP 端点 `POST /mcp`，所有 Agent 共享同一份采样与 ETW 会话：

```json
{
  "mcpServers": {
    "reshmon": { "url": "http://127.0.0.1:8964/mcp" }
  }
}
```

**B. stdio（serve 未运行时的独立兜底）**——内嵌只读采样（不落盘、不抢数据目录写锁），多个 Agent 各自启动互不干扰：

```bash
D:\Develop\Projects\reshmon\bin\reshmon.exe mcp
```

```json
{
  "mcpServers": {
    "reshmon": {
      "command": "D:\\Develop\\Projects\\reshmon\\bin\\reshmon.exe",
      "args": ["mcp"]
    }
  }
}
```

暴露 5 个工具：`resource_snapshot`（健康快照）、`agent_usage`（Agent 账本）、`top_processes`（进程 Top）、`resource_trend`（趋势摘要）、`health_events`（状态迁移）。之后 Agent 就能在发起大构建/并发任务前自己调 `resource_snapshot` 看余量，用 `agent_usage` 看自己和同伴各占多少，再决定开多大并发。stdio 版与 serve 同时运行时，ETW 会话按「附加消费」共享（多实例不拆台，仅首个实例负责回收）。

### 5. report —— 事后回溯（"昨晚三点是谁把磁盘打满了"）

```bash
reshmon.exe report                      # 今天 0 点到现在
reshmon.exe report -last 24h            # 最近 24 小时
reshmon.exe report -day 2026-10-25      # 指定某天
```

输出：总体健康分布 → 每维度 Top3 峰值窗口（30s 滑动均值）→ **每个峰值时刻的 Agent 归因 + 进程证据** → Agent 汇总（均值/峰值）→ 事件时间线。

## 数据与配置

数据目录默认 `%LOCALAPPDATA%\reshmon`（`serve`/`live`/`report` 不加 `-data` 时）。首次运行自动生成两个配置文件：

**`config.json`**——采样与保留策略（留空字段用默认值）：

```json
{
  "intervalMs": 2000,
  "procEverySec": 30,
  "retentionDays": 14,
  "etwIO": true,
  "etwCPU": true
}
```

`etwIO`：短命进程 IO 归因（ETW 内核事件，精确配对）；`etwCPU`：短命进程 CPU 比例归因（SampledProfile 采样）。均默认开、需管理员运行才实际生效，非管理员自动降级并在面板/TUI/MCP 的 errors 里明示。窗口/频率以**时长**为源：判定窗口恒约 30s、进程切片恒约 30s、环形缓冲恒约 2 小时——改 `intervalMs`（或 `-interval`）不会让这些承诺漂移。配置文件解析失败时工具以默认值继续跑，但会在 stderr 与面板上报 ⚠ 告警（不会静默失效）。

**`agents.json`**——Agent 识别规则（追加在内置规则之后；`match` 默认匹配进程名小写子串，`path:` 前缀匹配完整路径，`name:` 前缀精确匹配进程名）：

```json
{
  "agents": [
    { "match": "myagent", "label": "我的Agent" },
    { "match": "path:D:\\AI\\runtime", "label": "本地推理" }
  ]
}
```

内置识别：zcode、claude、codex、cursor、windsurf、trae、augment、cline、gemini、copilot、qwen、kimi、droid、qoder、ollama、lmstudio、llamafile、llama-server/llama.cpp、msty、anythingllm、chatbox、comfyui、sd-webui/stable-diffusion。

**落盘文件**（目录内）：`totals-YYYYMMDD.jsonl`（每 tick 总量+Agent）、`procs-YYYYMMDD.jsonl`（每约 30s 进程切片，四维度 Top 并集 100 条）、`incidents.jsonl`（状态迁移事件，面板从内存读、启动时回放）。**分层保留**：原始层（2s 粒度）保留 `retentionDays` 天（默认 14）；过期日不删除而是降采样为分钟级均值 `totals-min-YYYYMMDD.jsonl.gz`（体积约 1/30）再保留一年——`report` 长区间（如 `-last 90d`）自动混读两层，进程切片不降采样（到期即删）。跨天自动 gzip，超期自动清理；启动时自动补压缩"没跨过午夜"的遗留 raw 并修复崩溃残留的半截 .gz。环形缓冲保内存最近约 2 小时（面板用）。同一数据目录只允许一个写实例（`reshmon.lock`，持有者退出后自动接管）。

**健康阈值**（config.json 的 `thresholds` 字段可逐项覆盖，非零生效）：

| 维度 | 亚健康 | 饱和 |
|---|---|---|
| CPU 均值 | ≥65% | ≥85% |
| 物理内存 | ≥85% | ≥92% |
| 提交内存 | ≥88% | ≥95% |
| 磁盘忙碌 | ≥75% | ≥90% |
| 磁盘延迟 | ≥20ms | ≥45ms |
| 磁盘剩余空间 | ≤10% | ≤5% |
| GPU 利用率 | ≥80% | ≥95% |
| 显存占用 | ≥88% | ≥96% |
| CPU 温度（ACPI） | ≥90℃ | ≥100℃ |

判定用最近约 30s（按间隔换算 tick 数）的滑动均值：单帧尖峰不打扰，持续高位才升级；带 3% 滞回带——均值在阈值附近小幅波动不会来回翻转刷事件。实例缺席的 tick 不摊薄均值（拔盘后重新插入不会把持续打满的盘"平均"成健康）。GPU/磁盘的健康维度用稳定身份键（GPU=设备名键——LUID 每次开机重新生成，跨重启序列不断裂；磁盘=物理盘号），盘符变化/命名失败不会拆裂事件序列。状态迁移事件带 30s 最小驻留确认：秒级的往返脉冲（负载骤起骤停）不落事件，跳档时原档立即确认——事件时间只记真实持续的异常，且时间戳为迁移发生时刻。

## 指标口径（读数前须知）

- **进程 CPU%** 为占全机容量的百分比（全进程之和≈总体使用率）；单进程上限 100。分母取全机逻辑处理器数（跨处理器组），不受本工具自身亲和性影响。
- **CPU 温度**取自 ACPI 热区（`Thermal Zone Information`），是**封装附近**的温度，不是核心温度——读数通常比核心温度低，所以阈值定得比核心温度口径保守（90/100℃）。没有 ACPI 热区的机器不产生温度维度；读数自启动以来从未变化时会标注"可能为固定值"（部分主板只给固定值）。**核心温度/每核温度需要内核驱动读 MSR**，与本工具"零依赖、不装驱动"的定位冲突，故不提供（Argus 等工具是靠 PawnIO 内核驱动实现的）。
- **CPU 功耗**取自 Windows `Energy Meter`：封装/核心/核显三个域，单位与换算以 Windows 自带计数器帮助文本为准（`Power` 为每个采样间隔的平均功率，毫瓦）。同一读数的别名实例（如 "Apu Power" 与 "Socket Power"）只取其一、不相加；认不出的实例名不给数值，不做猜测。
- **GPU 利用率**是各引擎（3D/Compute/Copy/VideoDecode…）之和，单进程可超 100（多引擎并行），显示时截断；核显（如 AMD 860M）"显存"为 BIOS 划分的专用份额（常 512MB），大头上共享系统内存——看"共享占用"。进程显存归因为专用+共享之和。**WARP（Microsoft Basic Render Driver，CPU 软件渲染回退）不计入 GPU 口径**——它的活是 CPU 干的，计入会凭空多出一张幻影卡片。
- **Agent 内存**按私有提交（Private Bytes）求和，避免工作集把共享 DLL 重复计入。
- **短命进程归因（ETW，需管理员）**：**IO** 走内核 DiskIO 事件精确配对——存活不足一个采样间隔的进程（编译/链接的一次性 IO）也进 Agent 账本；**CPU** 走 SampledProfile 采样做比例归因——每拍「总 CPU% − Σ已差分进程」的缺口按采样占比分配给短命/首拍/受保护进程（统计估计值、免校准，Agent 账本之和收敛到总量；DPC/ISR 等无样本残余不硬摊）。死者的整段账目记在退出后那一拍（速率偏高、总量正确）。非管理员自动降级并在 errors 明示；`etwIO`/`etwCPU` 可分别关闭。
- **磁盘忙碌** = 100 − %Idle Time；**磁盘剩余空间**按卷用 GetDiskFreeSpaceExW 直取（% Free Space 计数器的 RAW 值语义跨版本漂移，弃用），物理盘卡片显示其最差卷；受保护进程的明细需管理员权限（IO 明细在管理员下由 ETW 补齐）。
- **网络总量**剔除虚拟适配器（Hyper-V vNIC/WSL/TAP 等）——同一流量会同时过 vNIC 与 pNIC 两个实例，全量求和会翻倍；明细表仍展示全部网卡。
- 每核负载只在 `live` 面板显示为 ▮ 色条。

## 已知局限

- **>64 逻辑处理器（多处理器组）**：总体 CPU 口径待真机验证（`\Processor(*)` 在多组机器上可能只枚举组 0）；进程 CPU% 分母已用 GetActiveProcessorCount(ALL_PROCESSOR_GROUPS) 对齐。
- **NVMe 的"队列"**计数器在新存储栈上常年显示 0（系统侧如此），判据只用忙碌%与延迟。
- **GPU 引擎首拍/末拍**：新出现的引擎实例首拍无基线记 0，进程退出瞬间的 GPU 贡献丢失一拍（与任务管理器同类的固有口径）。
- **NVIDIA 温度**按设备名与 DXGI 对齐，名称不可得时按稳定顺序配对；温度读取失败显示为空（-1 语义），不伪装成 0℃。
- **ETW 归因的边缘**：父链在会话启动前已全部死亡的短命进程只能按镜像名落桶（拿不到祖先 Agent）；分页 IO 按 IRP 发起上下文归账（多为触发进程）。ETW 需管理员，非管理员自动降级并在 errors 中明示。
- **amd64 only**：PDH/ETW 的 FFI 结构体按 x64 布局手写并有编译期断言（`internal/syswin`）。

## 典型用法：定位"磁盘被谁打满"

1. 平时挂着 `reshmon serve`（或开机自启，见下）
2. 机器卡了/事后复盘：`reshmon report -last 24h`
3. 找到"磁盘 C: D:"峰值窗口 → 看"当时 Agent"与"当时进程"两行，直接给出嫌疑人
4. 需要更细的曲线：面板 http://127.0.0.1:8964/ 切"磁盘忙"趋势 + 事件时间线

## 以 Windows 服务运行（推荐，替代开机自启计划任务）

管理员终端执行一次：

```powershell
D:\Develop\Projects\reshmon\bin\reshmon.exe service install
D:\Develop\Projects\reshmon\bin\reshmon.exe service start
```

服务特性：自动启动（延迟启动不拖慢开机）、崩溃由 SCM 自动重启（5s/30s/60s 递增）、登录前即运行、services.msc 可见可管理。数据目录在 install 时固化为当前用户的默认目录，避免 LocalSystem 下路径漂移；**升级版本只需覆盖 exe 后 `reshmon.exe service stop && reshmon.exe service start`**。

```powershell
reshmon.exe service status    # 运行中 (pid 1234)
reshmon.exe service stop      # 停止（保留注册）
reshmon.exe service remove    # 停止并删除
```

普通权限运行 `serve`/`live` 与服务并存时，后者会因数据目录锁自动降级为只看不记录（面板有提示）。若此前配置过开机自启计划任务，迁移：`schtasks /Delete /TN reshmon /F`。

## 技术路线

- 全部数据源走 Win32 原生 API：PDH 性能计数器（**RAW 路线**，相邻帧差分自算速率——`PdhGetFormattedCounterArray` 在部分系统上行为异常，`PdhGetRawCounterArray` 稳定）、Toolhelp32 + OpenProcess（进程 CPU/内存/IO）、GlobalMemoryStatusEx/GetPerformanceInfo（内存）、DXGI（GPU 适配器命名/LUID 映射）、NVML（NVIDIA 温度，可选）、**ETW 内核事件**（短命进程 IO：DiskIO Init/完成按 IRP 配对 + Process_V2 身份追踪，`internal/syswin/etw.go`，FFI 布局有编译期断言）
- MCP 服务器手写 JSON-RPC 2.0 over stdio（`internal/mcp`），零第三方依赖
- 计数器一律用英文名添加（PdhAddEnglishCounter），中文系统不受影响
- 本工具自身开销：2s 一拍 ≈ 每拍一次 PDH 采集 + 一次进程枚举，常态 CPU <1%、内存 ~20MB；ETW 会话（管理员下）仅在磁盘事件回调里做 map 计数，开销可忽略

## 开发

```bash
go build ./... && go vet -unsafeptr=false ./... && gofmt -l .   # 必须无输出
go test ./... -race
```

仅支持 Windows amd64（业务包带 `//go:build windows`）；`internal/report` 与 `internal/store` 跨平台纯逻辑。vet 关闭 `unsafeptr` 仅因 ETW 回调的 C-ABI 边界（uintptr→unsafe.Pointer 无法静态证明，布局由断言钉死）。CI（GitHub Actions windows-latest）跑 fmt/vet/build/test -race，见 `.github/workflows/ci.yml`。

## 路线图（未做，按需立项）

- GPU 温度/功耗（NVIDIA 之外）：CPU 的封装温度与功耗已提供（ACPI 热区 / Windows Energy Meter，v0.7.0）；AMD/Intel 显卡的温度与功耗需要各自的厂商 SDK（ADL / IGCL），尚未接入
- 核心温度 / 每核温度：需要内核驱动读 MSR，与本工具"零依赖、不装驱动"冲突，不做
- 磁盘温度与 NVMe 健康日志：温度查询没有超时、用户态无法取消，厂商存储驱动（Intel RST/VMD）上有查询不返回导致系统停止响应的公开案例。要做的话只应向 Windows 自带驱动（`stornvme`/`storahci`/`atapi`）管理的磁盘发送，需先评估
- MCP 增加 `concurrency_advice` 类决策辅助工具（目前刻意只给事实，决策留给 Agent）
