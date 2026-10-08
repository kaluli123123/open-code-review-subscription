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

### Switch providers and models quickly

Change the persistent default with `ocr config set`:

```sh
# Claude subscription
ocr config set provider claude-cli
ocr config set model opus       # or sonnet / haiku

# Codex subscription
ocr config set provider codex-cli
ocr config set model default    # use the model selected by the official Codex CLI
```

To use a different model for one command without changing the default:

```sh
ocr review --provider claude-cli --model sonnet
ocr scan --provider claude-cli --model opus
ocr review --provider codex-cli --model default
```

For example, if the official Codex CLI is configured with `gpt-5.6-luna`,
using `--model default` makes the subscription backend use that CLI-selected
model. The exact model availability is controlled by the official CLI and the
signed-in account.

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

## Upstream synchronization (2026-10-08)

This fork incorporates Alibaba's `main` at
[`182898cf522da3d04157b422752d028417974e19`](https://github.com/alibaba/open-code-review/commit/182898cf522da3d04157b422752d028417974e19),
42 upstream commits after the original `85cecfe` base. The three original
subscription-backend commits are retained rather than replaced by upstream.

The update includes OpenRouter, unlisted built-in API model overrides,
two-turn `ocr llm test` tool verification, `ocr session rm`, F# and `.j2`
support, quoted/non-ASCII path handling, safer configuration updates, and
viewer and CI fixes. Upstream removed `OCR_CONFIG_PATH`; configuration uses
the standard home-directory path. CLI subscription authentication, isolation,
and rejection of API fallback remain unchanged.

The frontend and IDEA provider catalogs are regenerated from the merged Go
registry, including `claude-cli` and `codex-cli`. The frontend protocol type
accepts both. The upstream all-provider model-override regression uses
protocol-appropriate credentials; subscription providers must not receive API
keys or AWS settings. A redundant test that pinned the entire provider list
was removed; the existing sorted-order invariant remains covered.

Verification of this synchronization:

- Go packages passed race-enabled tests. The initial full run exposed the
  API-only fixture assumption above; after correcting that fixture, the only
  failing package (`internal/llm`) passed its complete race-enabled suite.
- `go vet ./...`, generated-catalog consistency, license headers, English-only
  source checks, and GitHub Actions SHA-pin checks passed.
- Frontend type checking, all 54 tests, and the production build passed.
- GitHub Actions, plugin-contract, Node launcher, and version-script tests passed.
- The built CLI listed OpenRouter and both subscription providers. A temporary
  repository preview selected Chinese-named Python and `.j2` files plus F#,
  while excluding `node_modules`.
- Codex CLI 0.161.0 completed the real `ocr llm test` connection and tool-call
  round trip. Claude Code 2.1.289 passed subscription authentication preflight,
  but live inference was unavailable in the verification environment; it is
  not claimed revalidated by this synchronization.

These checks do not constitute a live full-review/scan run, native IDE
extension validation, viewer visual testing, or cross-platform verification.
The command on the system PATH is not replaced by a source build; use
`dist/opencodereview` to run this synchronized checkout.

## Original subscription implementation verification

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
