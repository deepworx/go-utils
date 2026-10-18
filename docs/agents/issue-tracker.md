# Issue tracker: Linear

Issues and specs for this repo live in Linear: team **deep worx** (`DWX`), scoped by the `go-utils` label in the `Repo` label group. Use the Linear MCP tools (`mcp__plugin_linear_linear__*`).

## Conventions

- **Create an issue**: `save_issue` with `team: "DWX"`, `labels: ["go-utils", ...]`, `title`, and a Markdown `description`.
- **Read an issue**: `get_issue` with the identifier (e.g. `DWX-42`), plus `list_comments` for the discussion.
- **List issues**: `list_issues` with `team: "DWX"`, `label: "go-utils"`, and appropriate `state` filters. Filter further by triage label client-side.
- **Comment on an issue**: `save_comment` on the issue.
- **Apply / remove labels**: `save_issue` with `id` and `addLabels` / `removeLabels`. Never use `labels`: it replaces the full set and would drop `go-utils`.
- **Close**: `save_comment` with the reason, then `save_issue` with `state: "Done"` (`"Canceled"` for wontfix, `duplicateOf` for duplicates).

Workflow states: Backlog → Todo → In Progress → In Review → Done (also Canceled, Duplicate).

PR titles reference the issue identifier, e.g. `feat(nats): add retry logic (DWX-42)`.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external GitHub PRs as feature requests; `/triage` reads this flag.)_

When set to `yes`, triage external PRs with `gh pr list` / `gh pr view <number> --comments` / `gh pr diff <number>`, keeping only `authorAssociation` of `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, or `NONE`, and file a linked Linear issue for any accepted request.

## When a skill says "publish to the issue tracker"

Create a Linear issue in team DWX with the `go-utils` label.

## When a skill says "fetch the relevant ticket"

Run `get_issue` with the identifier, plus `list_comments`.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a single issue with **child** issues as tickets.

- **Map**: an issue labelled `go-utils` + `wayfinder:map`, holding the Notes / Decisions-so-far / Fog body.
- **Child ticket**: a sub-issue of the map (`save_issue` with `parentId: <map>`), labelled `go-utils` + `wayfinder:<type>` (`research`/`prototype`/`grilling`/`task`). Once claimed, the ticket is assigned to the driving dev.
- **Blocking**: Linear's native blocking relations (`save_issue` with `blockedBy` / `blocks`). A ticket is unblocked when every blocker is in a completed or canceled state.
- **Frontier query**: `list_issues` with `parentId: <map>`, keep open ones (not Done/Canceled/Duplicate), drop any with an open blocker or an assignee; first in map order wins.
- **Claim**: `save_issue` with `assignee: "me"` and `state: "In Progress"`, the session's first write.
- **Resolve**: `save_comment` with the answer, then `save_issue` with `state: "Done"`, then append a context pointer (gist + link) to the map's Decisions-so-far.
