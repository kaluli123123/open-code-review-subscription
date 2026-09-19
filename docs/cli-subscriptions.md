# Subscription CLI backends

This is an experimental feature of an independent fork of
[alibaba/open-code-review](https://github.com/alibaba/open-code-review), not an
Alibaba, Anthropic, or OpenAI release or endorsement. Upstream copyright notices
and the [Apache-2.0 license](../LICENSE) are retained. The upstream npm package
does not install this fork's backends.

These backends adapt OCR's native `LLMClient`. They are not delegation mode:
OCR still runs review/scan orchestration, grouping, compression, filtering,
relocation, tool execution, and session history. Each model request is sent to
an official CLI as a portable transcript. The CLI returns the next assistant
response and tool-call data; OCR executes the tools.

## Scope and status

- `claude-cli`: local Claude Code subscription adapter. Requires the official
  CLI with safe-mode, disabled built-in tools, and structured JSON output.
- `codex-cli`: official Codex CLI app-server adapter, using experimental
  no-environment threads/turns rather than `codex exec`. Requires Codex
  0.153.4 or later, strict configuration, and verified effective isolation
  settings; incompatible versions or settings must fail closed.
  It does not fall back to an API or to delegation.
- Existing API protocols remain available for users who explicitly choose them.

This is a compatibility adapter, not a byte-for-byte implementation of a
provider's API protocol. Do not represent it as an equivalent benchmark result.

## Build and use

Use macOS or Linux. Windows is not supported by these CLI backends. Real CLI
smoke tests were run on macOS; Linux has not been verified with a live account.
Install the official Claude Code or Codex CLI separately and sign in through
that CLI. OCR does not import, copy, or refresh your subscription credentials.

Build this source checkout using the Go toolchain required by `go.mod`:

```sh
make build
./dist/opencodereview --help
```

Until explicitly installed, `ocr` on your PATH still refers to your existing
installation. Run reviews from the repository you want to review, using the
path to the binary you just built (replace the example path below).

With your own Claude subscription already signed in through the official CLI:

```sh
/path/to/open-code-review/dist/opencodereview review --provider claude-cli --model sonnet
```

For Codex, sign in through the official CLI using ChatGPT, then select its
backend explicitly (the CLI selects its default model unless overridden):

```sh
/path/to/open-code-review/dist/opencodereview review --provider codex-cli
```

These commands use the native OCR review pipeline, not `ocr delegate`.
Run them in the repository you want to review. Do not supply an API key, API URL,
key command, custom HTTP headers, or a third-party model route for this backend.
Authentication changes are performed only through the official CLI, not OCR.

The backend must reject conflicting API configuration, invalid authentication,
unsupported CLI capabilities, malformed output, quota errors, and timeouts.
Codex also rejects a custom `model_catalog_json`: model catalogs can change
native code-mode and multi-agent capabilities, not just model labels. The
adapter does not edit this user setting. If the preflight reports this conflict,
review the setting and explicitly choose the official default catalog before
using this backend. Do not bypass this guard just to get a successful run.
It never retries via an API provider. Subscription quotas still apply; a
subscription is not unlimited inference. Provider-side extra-usage billing,
if enabled on the account, is outside OCR's control: disable it in the account
if a strict no-extra-charge policy is required.

## Compatibility limits

- Each call starts a new isolated CLI session using OCR's current history.
  CLI resume/continue is not used. OCR's context compression remains in charge.
- Hidden provider reasoning/signatures are not replayed. Do not assume API
  sessions can be resumed transparently with a CLI backend.
- Only the supported text and tool-result transcript formats are accepted.
  Unsupported multimodal content must fail rather than be silently discarded.
- API temperature and output-token controls are not equivalent to CLI options.
  `MaxTokens` is a soft request, not a hard limit on subscription consumption.
- CLI startup, schema handling, and repeated transcripts add latency and quota
  use. Usage accounting is based on CLI output, not model-generated counters.
  A failed call may have consumed subscription quota before its final usage
  arrives. Zero tokens in a failed OCR report is not proof of zero consumption;
  cancellation cannot undo an already submitted inference request.
- HTTP raw-capture and per-HTTP-attempt retry telemetry do not describe CLI
  internals. Do not infer zero model usage from an empty HTTP retry report.
- The initial scope is local, single-user operation. Do not upload login caches,
  pool personal subscriptions, or assume a server/CI inherits laptop login.

## Known process-cleanup limitation

POSIX process-group cleanup is best effort, not an OS sandbox. After the CLI
leader has been reaped, there is a narrow PID/PGID reuse race: the final group
cleanup could target an unrelated process group if its identifier was reused.
This is a known limitation of this experimental version, not a fixed or
reproduced incident. Removing that cleanup would leave some descendants alive
after a successful parent exit, so it remains in place pending dedicated
process-lifecycle hardening. Use only trusted official CLIs in local,
single-user environments. Shared or production-grade isolation requires that
hardening first.

## Verification

The implementation was verified with `make test`, `make check`, and
`make coverage`; total coverage was 92.3%. Fake CLI tests cover authentication,
argv/stdin, transcript replay, native OCR tool rounds, no-tool auxiliary
requests, invalid responses, timeouts, and bounded output. Fake tests cannot
prove real subscription authentication or model behavior.

Both Claude Code and Codex completed a real native OCR review of a small,
intentional-bug fixture without API keys. These checks exercised the main
review/tool loop and the filtering stage. The reviewed fixture was not modified.
They do **not** establish live end-to-end coverage of grouping, compression,
relocation, or scan; those stages were not triggered by the small fixture.
They also do not establish production readiness or compatibility with every
model, account, operating system, or future CLI release.

The precommit OCR self-review was **partial**: 4 of 13 selected files completed;
9 stopped at the token-budget limit. It reported four findings (two shared one
timeout root cause). The queue timeout and provider documentation/test gaps were
addressed; the process-cleanup race above remains an accepted experimental
limitation. Independent blue/red source review covered the remaining nine
files. This supplements the evidence but does not turn the OCR run into a
complete or clean automated review. The corrected snapshot is checked with
local tests and independent source review, not another paid full OCR run.

## Isolation protocol references

The implementation was checked against Claude Code 2.1.276 help and the Codex
0.153.4 protocol/source. This is not a claim that later releases have been
smoke-tested. Incompatible configurations fail closed; revalidate after CLI
upgrades.

- [Codex thread/start environment contract](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/app-server-protocol/src/protocol/v2/thread.rs)
- [Codex turn/start environment and output schema contract](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/app-server-protocol/src/protocol/v2/turn.rs)
- [Codex tool registration](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/core/src/tools/spec_plan.rs)
- [Claude non-interactive operation](https://code.claude.com/docs/en/headless)

No-environment Codex threads remove filesystem/command tools. Models may still
advertise non-environment native tools (for example, time information). OCR
rejects native tool events rather than accepting such a turn as its own tool
execution. MCP names are obtained from effective configuration before creating
a thread; status probing is performed only after disabling those servers.
