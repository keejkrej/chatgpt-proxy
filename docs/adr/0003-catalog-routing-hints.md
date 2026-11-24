# ADR-0003: Treat Codex catalogs as routing hints

- **Status:** accepted
- **Date:** 2026-11-24

## Decision

Prefer available accounts known to list the requested Responses model, but do not use discovery as an allowlist.

## Why

The model catalog depends on the account and the Codex client version. A temporary lookup error, a new model, or a hidden listing should not by itself stop a request with an explicit model name. Fetch catalogs from the discovery endpoint rather than adding network latency to inference requests.

## Looks like a bug when

A model absent from `/v1/models` can still be requested. The proxy leaves the final availability decision to Codex.
