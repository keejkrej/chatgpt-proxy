# ADR-0001: Delegate sign-in to Codex

- **Status:** accepted
- **Date:** 2026-11-24

## Decision

Run the official Codex sign-in in an isolated temporary home per login, save the resulting account file locally, and read it anew for each request. Do not implement independent token refresh.

## Why

This keeps ChatGPT OAuth out of the proxy and avoids touching the user's normal Codex credentials. The proxy does not need to manage rotating refresh tokens or coordinate refresh writes. The trade-off is that expired credentials must be renewed by running `chatgpt-proxy login` again.

## Looks like a bug when

A regular Codex login or refresh doesn't update the proxy's accounts, or an expired proxy session does not refresh on its own. Run `chatgpt-proxy login` again; the same account's stored credentials are replaced.
