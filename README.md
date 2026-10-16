# ARIES-browser-bench

这个仓库 fork 自 [ARIES](https://github.com/hyscale-lab/ARIES)（Agent Runtime & Infrastructure Experimentation System），在它的基础上加了一套**浏览器类 sandbox-heavy 负载**的实验环境：

- 浏览器（Chromium + [Playwright MCP](https://github.com/microsoft/playwright-mcp)）跑在 ARIES 的 task sandbox 里；
- 网站是真实的公网；
- 模型是真实模型（目前用 DeepSeek）；
- 任务来自 [AssistantBench](https://arxiv.org/abs/2407.15711) 的 dev 集，评分用的是 AssistantBench 官方评分器的 Go 移植。

这样的拓扑和生产环境里的 agent 一致：浏览器的渲染、JS 执行、无障碍树生成都在 sandbox 里完成，网站的后端不在本机。ARIES 原有的生命周期、隔离、证据记录和资源采样都可以直接复用。

> ARIES 本身的设计、组件契约和其他 benchmark 请看[上游仓库](https://github.com/hyscale-lab/ARIES)和本仓库的 [`docs/`](docs/)。本 README 只介绍这个 fork 新增的内容。

## 新增内容

| 内容 | 位置 |
| --- | --- |
| `browserbench` benchmark 适配器：加载任务、在 sandbox 里启动 Playwright MCP、在宿主侧评分 | [`pkg/benchmark/browserbench/`](pkg/benchmark/browserbench/) |
| AssistantBench `question_scorer` 的 Go 移植（按 BrowserGym `f0e4275` 固定的版本），并用 2432 条上游 Python 结果做逐条比对 | [`score.go`](pkg/benchmark/browserbench/score.go)、[`pyvalue.go`](pkg/benchmark/browserbench/pyvalue.go)、[`testdata/`](pkg/benchmark/browserbench/testdata/) |
| 配置项 `benchmark.browserbench`，数据集按 revision 和 SHA-256 固定 | [`pkg/config/config.go`](pkg/config/config.go)、[`configs/versions.json`](configs/versions.json) |
| 5 道题的 Hermes + DeepSeek profile | [`profiles/hermes-browserbench-ab5-deepseek.json`](profiles/hermes-browserbench-ab5-deepseek.json) |
| Hermes 修正：不开 web_search 时，关闭 Hermes 自带的浏览器和搜索工具 | [`pkg/harness/hermes/config.go`](pkg/harness/hermes/config.go) |
| 使用说明和实现说明 | [`docs/benchmarks/browserbench.md`](docs/benchmarks/browserbench.md)、[`docs/implementation/benchmarks.md`](docs/implementation/benchmarks.md#browserbench) |

## 拓扑

```
harness 容器（Hermes）──────────► DeepSeek API（公网）
   │  SSH bridge：shell 工具，用来写答案文件
   │  MCP（streamable HTTP）：服务名 `playwright`
   ▼
sandbox 容器（mcr.microsoft.com/playwright/mcp:v0.0.83）
   Playwright MCP + headless Chromium ─────────► 公网网站
```

- sandbox 镜像直接用 Playwright MCP 官方镜像，有 amd64 和 arm64 两种架构。ARIES 会把镜像的 entrypoint 换成 `sleep infinity`，所以由 `PrepareSandbox` 在后台启动 MCP 服务，等它开始监听后再放 harness 进来。
- harness 通过 run network 上 ARIES 解析出的地址连接 `playwright` 服务。工具返回的是无障碍树快照（纯文本），所以没有视觉能力的模型也能浏览网页。
- agent 把答案写进 `/tmp/aries-answer.txt`。评测在 bridge 撤销之后，从 sandbox 下载这个文件，在宿主侧和标准答案比对。标准答案从不进入 sandbox。
- `score` 记 AssistantBench 的部分分（0–1）；只有完全答对时，`reward` 才是 1，状态才是 `succeeded`。

## 在 Mac 上运行

ARIES 要求 Linux 和本地 Docker Engine，所以在 Mac 上要用一台 Linux 虚拟机。下面以 [OrbStack](https://orbstack.dev) 为例，仓库放在 macOS 上，由 OrbStack 自动挂载进 VM。

```bash
orb create ubuntu:noble aries-vm
```

在 VM 里安装 Docker Engine（用 Docker 官方 apt 源，API 版本需要 ≥ 1.56）、Go 1.26.5、git 和 make，并把当前用户加入 `docker` 组。然后在 VM 里：

```bash
make build && make bridge-image
```

DeepSeek 密钥放在仓库根目录的 `DEEPSEEK_API.key`（已被 git 忽略），写入方法见 [`docs/configuration.md`](docs/configuration.md#external-deepseek)。准备数据和镜像，然后运行：

```bash
./bin/aries setup profiles/hermes-browserbench-ab5-deepseek.json
```

```bash
./bin/aries profiles/hermes-browserbench-ab5-deepseek.json
```

运行会产生 DeepSeek 费用。结果在 `runs/<run-id>/`，每题的 `evaluation/score.json` 记录了预测、标准答案和 accuracy，`monitor/resources.jsonl` 记录了 harness 和 sandbox 的 CPU、内存采样。

## 第一次实验结果

环境：Apple Silicon Mac，OrbStack VM（10 核、12GB），Hermes v2026.8.31 + `deepseek-flash`，concurrency 1，容器不设资源上限，2026-10-15。

| 题目（AssistantBench dev） | 用时 | accuracy | sandbox 内存峰值 | sandbox CPU | harness CPU | 浏览器工具调用 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 西雅图 9 月初雨天概率 | 933 s | 0.658 | 866 MiB | 29 s | 115 s | 29 |
| 休斯顿 6 月超 95°F 概率 | 70 s | 0.766 | 835 MiB | 20 s | 14 s | 9 |
| 芝加哥新年夜降雪概率 | 381 s | 0 † | 876 MiB | 67 s | 57 s | 29 |
| Vanguard ETF 涨幅比较 | 48 s | 1.000 | 2057 MiB | 30 s | 10 s | 9 |
| Fidelity 新兴市场基金涨幅 | 704 s | 0 † | 1539 MiB | 132 s | 122 s | 51 |

† agent 用完 Hermes 的 90 轮上限，答案只出现在聊天回复里，没写进文件，所以按 0 分计。其中芝加哥那题回复里的答案与标准答案一致。

对照：同一环境下的纯 shell 任务（Terminal-Bench 2 `fix-git`），sandbox 内存峰值 215 MiB、CPU 13 s。加入浏览器后，sandbox 的内存峰值是它的 4–10 倍。

## 实验过程中发现的问题

- **Hermes 自带的浏览器会绕过 sandbox。** Hermes v2026.8.31 即使收到显式的 toolset 列表，也会把自带的 `browser` toolset 补回来；其中的 `browser_exec` 和 `web_search` 都在 harness 容器里执行。现在不开 `harness.web_search` 时，ARIES 会禁用 `browser`、`browser-cdp` 和 `web`。开了 web_search 时只禁用 `browser-cdp`，因为 `web_search` 同时属于 `browser` toolset；这种情况下 `browser_exec` 仍然可用，测 sandbox 负载时不要开 web_search。
- **MCP 服务名不能和 toolset 撞名。** Hermes 会把 MCP 服务的 toolset 登记成服务名的别名，所以服务名叫 `browser` 时，会被禁用 `browser` toolset 一起关掉。因此服务名改成了 `playwright`。
- **agent 会绕开浏览器。** 如果不加限制，agent 会在 sandbox 里写 node 脚本直接调用数据 API。现在指令要求只能用浏览器工具找答案，shell 只用来写答案文件，这和上游 BrowserGym 只提供浏览器动作的设定一致。
- **Playwright MCP 0.0.83 的 `browser_navigate` 不直接返回快照**，只返回快照文件的链接。所以 agent 常常把快照存成文件，再用 shell 去 grep。

## 注意事项

- 公网会变化。AssistantBench 的答案是 2024 年采集的，很多题依赖实时信息。这个 profile 选的 5 道题都依赖 2013–2024 年的历史数据，但成功率仍然只适合做参考。
- 只有 dev 集的 33 道题公开了答案；test 集的 181 道题答案不公开。
- 网站可能拦截自动化浏览器；适配器不做任何绕过。
- 目前只在 Hermes 上验证过；OpenClaw 自带的工具是否也会绕过 sandbox，还没有检查。

## 后续计划

1. **`shm_size`。** Docker 默认的 `/dev/shm` 只有 64MB，需要在 `core.Environment` 和 Docker deployment 里支持设置它。
2. **扩展资源指标**：pids、shmem、`memory.peak`、PSI 压力指标、网络字节数。
3. **网络时序证据**，用来把"等远端网站"和"本地渲染、执行 JS"分开计算。
4. **处理答案只出现在聊天回复的情况**：调高轮数上限，或者额外记录 harness 的最终回复。
5. **扩展任务和实验维度**：加入 BrowseComp 子集；扫并发数、浏览器类型、资源限制。

## 许可

代码沿用 ARIES 的 [MIT 许可](LICENSE-CODE)，数据集和文档沿用 [CC-BY-4.0](LICENSE)。AssistantBench 数据集为 Apache-2.0，由 `aries setup` 下载到 `.cache/`，不在仓库里。评分器移植自 BrowserGym 收录的 AssistantBench 评测代码，出处写在源码注释里。
