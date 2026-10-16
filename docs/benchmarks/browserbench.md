## browserbench

`profiles/hermes-browserbench-ab5-deepseek.json` runs five
[AssistantBench](https://arxiv.org/abs/2407.15711) dev tasks with a browser
that runs in the task sandbox. It is meant for studying sandbox-heavy agent
workloads: the sandbox carries the browser's work, the websites are the public
internet, and the model is a real one.

## Topology

- The task image is [Playwright MCP](https://github.com/microsoft/playwright-mcp)'s
  own, pinned by tag in the profile (`mcr.microsoft.com/playwright/mcp:v0.0.83`,
  which has `linux/amd64` and `linux/arm64` builds). Preparation starts its
  server in the sandbox with headless Chromium and an in-memory profile.
- The harness reaches the server as the MCP server named `playwright`, over
  streamable HTTP on the run network. Its tools return accessibility snapshots
  as text, so a model without vision can browse. In 0.0.83,
  `browser_navigate` returns a link to the snapshot file rather than the
  snapshot, so a model usually follows it with `browser_snapshot`.
- The harness keeps its shell tools: the agent writes its answer to
  `/tmp/aries-answer.txt` with them. The instruction confines research to the
  browser, as upstream's browser-only agents are; an agent may still fetch from
  the shell, which the bridge's `tool-calls.jsonl` shows. Leave
  `harness.web_search` disabled, or some browsing happens in the harness
  rather than the sandbox: Hermes's own browser tools also run in the
  harness, and ARIES disables them only while web search is off (see the
  [Hermes notes](../configuration.md#harness-settings)).
- The sandbox has network access. Websites are not part of the experiment's
  resources and are not observed.

## Tasks and scoring

Task IDs are AssistantBench IDs from the dev split, the only split that
publishes answers (33 tasks; the 181 test answers are withheld). `aries setup`
downloads the dev file from the Hugging Face dataset at the revision pinned in
`configs/versions.json` and checks its SHA-256, which task loading and
evaluation check again.

Upstream, the agent receives the bare question and its final chat message is
scored. ARIES scores a file, so the instruction adds where to write the answer
and the shapes the scorer parses: a bare number, or a JSON array of strings
for several items. Evaluation scores the answer on the host with a port of
AssistantBench's `question_scorer` as vendored by
[BrowserGym](https://github.com/ServiceNow/BrowserGym) at
`f0e4275d706f31aa341e58e49a5effbb52a392a8`:

- `score` is the scorer's accuracy, with partial credit in [0, 1];
- `reward` is 1, and the evaluation status `succeeded`, only for accuracy 1;
- a missing answer scores 0, as do inputs on which the Python scorer raises
  or returns a non-finite value, with the reason in the evaluation's `error`.

`evaluation/score.json` records the prediction, the gold answer, the accuracy
and any scorer error; `evaluation/browser-mcp.log` is the server's log.

## Timing and resource evidence

With `network_timing` on (the default), preparation installs a small script
that Playwright MCP loads into every page (`--init-page`). It appends one JSON
line per finished or failed request (DNS, connect, TLS, request and response
offsets, status, sizes) and per page load (navigation timing) to
`/tmp/aries-browser/network.jsonl`. Evaluation keeps it as
`evaluation/browser-network.jsonl` and writes `evaluation/network-summary.json`:
request counts, response bytes, `network_active_ms` (the union of request
lifetimes), and, for each page load, the document's network time and the time
after its last byte.

To split a task's browser time between waiting on remote sites and local work,
join those records with Hermes's spans:

```sh
go run ./scripts/browser-timing runs/<run-id>
```

For each task it reports the browser tool calls' total time, the part in which
a request was on the network (`remote wait`), the rest (`local`: rendering,
script execution, snapshots and the tool protocol's overhead), network
activity between calls, and model time. All runtimes share the host clock, so
the browser's and the harness's timestamps compare directly. The script reads
Hermes's `telemetry/otel-spans.jsonl` and does not support OpenClaw.

The recording itself costs a little: one extra CDP query per request for its
sizes and one script evaluation per page load. Set
`benchmark.browserbench.network_timing` to `false` to measure without it.

`monitor/resources.jsonl` carries the optional Docker measurements described in
[run results](../run-results.md#measurement-availability), including process
count, shared memory, peak memory, network bytes and, on kernels with PSI,
pressure stall totals. `benchmark.environment.shm_size_mb` sizes the sandbox's
`/dev/shm`. Playwright launches Chromium with `--disable-dev-shm-usage`, so by
default Chromium keeps its shared memory in `/tmp` and the setting matters
little until that flag is removed.

## Caveats

- The public internet changes. AssistantBench's answers were collected in 2024,
  and many tasks depend on live listings, opening hours or availability. Prefer
  tasks anchored to past data (the profile's five ask about 2013–2024 records)
  and read accuracy with that in mind.
- Websites may block automated browsers or show CAPTCHAs; the adapter does not
  work around them.
- `blocked_origins` steers the browser away from pages such as answer leaks,
  but Playwright documents that it is not a security boundary and does not
  apply to redirects.
- The default task timeout is 30 minutes; `agent_timeout_seconds` in a
  runtime overrides file replaces it.
- Chromium runs without its own sandbox (the image's default); the container
  is the boundary.
- The timing records and the server log are written in the sandbox, where the
  agent's shell could alter them; they are evidence, not verifier input. The
  recording script itself is owned by root.
