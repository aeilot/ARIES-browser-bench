# ARIES-browser-bench

This repository is a fork of [ARIES](https://github.com/hyscale-lab/ARIES)
(Agent Runtime & Infrastructure Experimentation System). It adds an
experiment environment for **browser-based, sandbox-heavy agent workloads**:

- the browser (Chromium with [Playwright MCP](https://github.com/microsoft/playwright-mcp))
  runs in the ARIES task sandbox;
- the websites are the live public internet;
- the model is a real one (DeepSeek so far);
- the tasks are the [AssistantBench](https://arxiv.org/abs/2407.15711) dev
  split, scored by a Go port of AssistantBench's own scorer.

This matches how agents are deployed: rendering, JavaScript execution and
accessibility-tree generation happen in the sandbox, while the websites'
backends are elsewhere. ARIES's lifecycle, isolation gates, evidence and
resource sampling apply unchanged.

> For ARIES itself — its design, component contracts and other benchmarks —
> see the [upstream repository](https://github.com/hyscale-lab/ARIES) and this
> repository's [`docs/`](docs/). This README covers what the fork adds.

## What this fork adds

| Addition | Location |
| --- | --- |
| The `browserbench` benchmark adapter: task loading, starting Playwright MCP in the sandbox, host-side scoring | [`pkg/benchmark/browserbench/`](pkg/benchmark/browserbench/) |
| A Go port of AssistantBench's `question_scorer` as pinned by BrowserGym at `f0e4275`, checked case by case against 2,432 results recorded from the upstream Python | [`score.go`](pkg/benchmark/browserbench/score.go), [`pyvalue.go`](pkg/benchmark/browserbench/pyvalue.go), [`testdata/`](pkg/benchmark/browserbench/testdata/) |
| The `benchmark.browserbench` profile block; the dataset pinned by revision and SHA-256 | [`pkg/config/config.go`](pkg/config/config.go), [`configs/versions.json`](configs/versions.json) |
| A five-task Hermes + DeepSeek profile | [`profiles/hermes-browserbench-ab5-deepseek.json`](profiles/hermes-browserbench-ab5-deepseek.json) |
| A Hermes fix: without web search, Hermes's own browser and search tools are disabled | [`pkg/harness/hermes/config.go`](pkg/harness/hermes/config.go) |
| Usage and implementation notes | [`docs/benchmarks/browserbench.md`](docs/benchmarks/browserbench.md), [`docs/implementation/benchmarks.md`](docs/implementation/benchmarks.md#browserbench) |

## Topology

```
harness container (Hermes) ─────────────► DeepSeek API (public internet)
   │  SSH bridge: shell tools, used to write the answer file
   │  MCP over streamable HTTP: server "playwright"
   ▼
sandbox container (mcr.microsoft.com/playwright/mcp:v0.0.83)
   Playwright MCP + headless Chromium ────► public websites
```

- The sandbox image is Playwright MCP's official one, built for amd64 and
  arm64. ARIES replaces the image's entrypoint with `sleep infinity`, so
  `PrepareSandbox` starts the MCP server in the background and waits until it
  listens before the harness is admitted.
- The harness reaches the `playwright` server at the address ARIES resolves on
  the run network. Its tools return accessibility snapshots as text, so a model
  without vision can browse.
- The agent writes its answer to `/tmp/aries-answer.txt`. After the bridge is
  revoked, evaluation downloads that file from the sandbox and scores it
  host-side; gold answers never enter the sandbox.
- `score` is AssistantBench's partial-credit accuracy in [0, 1]. `reward` is 1,
  and the status `succeeded`, only for a fully correct answer.

## Running on a Mac

ARIES needs Linux and a local Docker Engine, so on a Mac it runs in a Linux VM.
The steps below use [OrbStack](https://orbstack.dev); the repository stays on
macOS and OrbStack mounts it into the VM.

```bash
orb create ubuntu:noble aries-vm
```

In the VM, install Docker Engine from Docker's apt repository (API 1.56 or
later), Go 1.26.5, git and make, and add your user to the `docker` group. Then,
in the VM:

```bash
make build && make bridge-image
```

Put the DeepSeek key in `DEEPSEEK_API.key` at the repository root (ignored by
git); see [`docs/configuration.md`](docs/configuration.md#external-deepseek)
for how to write it. Prepare the dataset and images, then run:

```bash
./bin/aries setup profiles/hermes-browserbench-ab5-deepseek.json
```

```bash
./bin/aries profiles/hermes-browserbench-ab5-deepseek.json
```

The run incurs DeepSeek charges. Results land in `runs/<run-id>/`. Each task's
`evaluation/score.json` records the prediction, the gold answer and the
accuracy; `monitor/resources.jsonl` records CPU and memory samples for the
harness and the sandbox.

## First results

Setup: Apple Silicon Mac, OrbStack VM (10 cores, 12 GB), Hermes v2026.8.31 with
`deepseek-flash`, concurrency 1, no container resource limits, 2026-10-15.

| Task (AssistantBench dev) | Duration | Accuracy | Sandbox peak memory | Sandbox CPU | Harness CPU | Browser tool calls |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Chance of rain in early September, Seattle | 933 s | 0.658 | 866 MiB | 29 s | 115 s | 29 |
| Chance of days over 95°F in June, Houston | 70 s | 0.766 | 835 MiB | 20 s | 14 s | 9 |
| Chance of snow on New Year's Eve, Chicago | 381 s | 0 † | 876 MiB | 67 s | 57 s | 29 |
| Largest increase among Vanguard ETFs | 48 s | 1.000 | 2057 MiB | 30 s | 10 s | 9 |
| Smallest increase among Fidelity emerging-markets funds | 704 s | 0 † | 1539 MiB | 132 s | 122 s | 51 |

† The agent used up Hermes's 90-turn limit and gave its answer only in its
chat reply, without writing the file, so it scores 0. For the Chicago task, the
answer in the reply matches the gold answer.

For comparison, a shell-only task in the same environment (Terminal-Bench 2
`fix-git`) peaked at 215 MiB of sandbox memory and used 13 s of sandbox CPU.
With a browser, sandbox peak memory is 4–10 times higher.

## Issues found along the way

- **Hermes's own browser bypasses the sandbox.** Hermes v2026.8.31 adds its
  `browser` toolset back even to an explicit toolset list; its `browser_exec`
  and `web_search` run in the harness container. ARIES now disables `browser`,
  `browser-cdp` and `web` when `harness.web_search` is off. With web search on,
  only `browser-cdp` is disabled, because `web_search` also belongs to the
  `browser` toolset; `browser_exec` then remains available, so leave web search
  off when measuring sandbox load.
- **An MCP server must not share a toolset's name.** Hermes aliases an MCP
  server's toolset to the server's name, so a server named `browser` was
  disabled together with Hermes's `browser` toolset. The server is therefore
  named `playwright`.
- **Agents route around the browser.** Left unconstrained, the agent wrote
  Node scripts in the sandbox to call data APIs directly. The instruction now
  confines research to the browser tools and the shell to writing the answer,
  matching upstream BrowserGym, whose agents have only browser actions.
- **Playwright MCP 0.0.83's `browser_navigate` does not return the snapshot**,
  only a link to a snapshot file, so agents often save snapshots to files and
  grep them from the shell.

## Caveats

- The public internet changes. AssistantBench's answers were collected in
  2024, and many tasks depend on live information. The five tasks in the
  profile ask about 2013–2024 records, but accuracy is still only indicative.
- Only the 33 dev tasks publish answers; the 181 test answers are withheld.
- Websites may block automated browsers; the adapter does not work around them.
- Only Hermes has been checked. Whether OpenClaw's own tools also bypass the
  sandbox has not been verified.

## Next steps

1. **`shm_size`.** Docker's default `/dev/shm` is 64 MB; `core.Environment`
   and the Docker deployment should let a profile set it.
2. **More resource metrics**: pids, shmem, `memory.peak`, PSI pressure, and
   network bytes.
3. **Network timing evidence**, to separate waiting on remote sites from local
   rendering and JavaScript execution.
4. **Answers left only in the chat reply**: raise the turn limit, or also
   record the harness's final reply.
5. **More tasks and dimensions**: a BrowseComp subset; sweeps over
   concurrency, browser build and resource limits.

## License

Code follows ARIES's [MIT license](LICENSE-CODE); datasets and documentation
follow [CC-BY-4.0](LICENSE). The AssistantBench dataset (Apache-2.0) is
downloaded to `.cache/` by `aries setup` and is not part of the repository. The
scorer is ported from the AssistantBench evaluation code that BrowserGym
vendors; the source comments give its provenance.
