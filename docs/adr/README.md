# Architecture Decision Records

Why a shipped choice looks the way it does. Agents write these during the change. The procedure is the `decision-record` skill.

| Status | Meaning |
| --- | --- |
| `accepted` | Current choice. Matching behavior is intended. |
| `proposed` | Not yet confirmed. Matching behavior is not protected. |
| `superseded` | Replaced. The old file stays, and it links to the new ADR. |

## Index

| ID | Status | Title |
| --- | --- | --- |
| [0001](0001-codex-owns-login.md) | accepted | Delegate sign-in to Codex |
| [0002](0002-sticky-routing.md) | accepted | Use cache metrics for cold routing only |
| [0003](0003-catalog-routing-hints.md) | accepted | Treat Codex catalogs as routing hints |
