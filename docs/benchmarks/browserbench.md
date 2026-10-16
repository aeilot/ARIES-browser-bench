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
