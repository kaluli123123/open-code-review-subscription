# Product Marketing Context

**Document version:** v1
**Last updated:** 2026-09-19

## Product overview

Independent Apache-2.0 derivative of Alibaba Open Code Review. Adds native
Claude Code and Codex CLI backends to OCR's existing model interface. OCR owns
review orchestration, history and tool execution; this is not delegation mode.
The implementation uses the user's installed official CLI and existing
subscription login, not an OCR-managed OAuth-token proxy.

## Audience and use case

Individual developers who already have eligible Claude or ChatGPT/Codex
subscriptions and want native OCR reviews without obtaining a provider API key.
This is a local, single-user developer tool, not a hosted multi-user service.

## Claims and evidence

- Both backends completed a real small native review and its filtering stage.
  Both identified the deliberately introduced arithmetic defect without changing
  the fixture file. This does not establish live coverage of every conditional
  grouping, compression, relocation or scan branch.
- Local `make check`, full `make test`, and `make coverage` passed. Coverage was
  92.3% at the validated development snapshot; this is not a permanent guarantee.
- Subscription mode rejects incompatible authentication/routing configuration
  and does not fall back to a paid API transport.
- CLI transports add overhead and do not reproduce all API parameter or hidden
  reasoning semantics. Model availability and quotas remain provider-controlled.

## Positioning and alternatives

The distinction is keeping OCR's own tool loop, unlike host-driven delegation.
Upstream PR #180 is a prior independent native CLI approach; upstream PR #1106
uses a different direct-OAuth architecture. Do not claim this fork invented
subscription review or is the only implementation. Upstream work is credited.

## Voice and exclusions

Use concrete technical language and reproducible setup commands. Clearly label
this project as independent and unofficial. Keep upstream license and copyright.
Never promise unlimited/free inference, guaranteed zero bills, platform approval,
production readiness, all-branch live verification, or support for untested
platforms. No customer logos, endorsements or testimonials are asserted.

## Goals

Help interested users evaluate and run the native CLI backends safely. Link to
setup, compatibility limits and verification evidence, not just promotional copy.
Any upstream comment should be relevant, disclose the author's relationship and
AI-assisted development, and avoid repeated cross-posting.

## Changelog

- v1 (2026-09-19) — Initial positioning and evidence boundaries for publication.
