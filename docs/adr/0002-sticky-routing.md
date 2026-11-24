# ADR-0002: Use cache metrics for cold routing only

- **Status:** accepted
- **Date:** 2026-11-24

## Decision

Keep sessions pinned while an account is available, and only use observed cache rates to choose accounts for requests without a current session binding.

## Why

Cached input token counts arrive after the model has processed a request. They measure past work, not the cache state for the same prefix on a different account. Switching a warm session after a miss can discard useful cache history and destabilize client state. Occasional sampling of cold requests gives the adaptive strategy a way to observe alternative accounts without replaying work.

## Looks like a bug when

A session stays on an eligible account despite worse cache telemetry than another account. That is intentional; cold routing can change, and an unavailable account can still be replaced.
