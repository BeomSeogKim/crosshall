# crosshall - agent guide

Rules for AI coding agents working in this repository. `CLAUDE.md` is a symlink
to this file; edit `AGENTS.md` only.

## Status

The core command-line tool is released as release candidates (see the tags
and `README.md`). Vendor adapters are being built: the hook command exists,
installing it into an agent does not yet. The project was called `sotto`
while it was being designed; a local folder or older notes may still use that
name.

## Private notes

- `.notes/` is ignored by git and holds local working notes: requirements,
  design discussion, experiments. Never add anything under `.notes/` to a
  commit, and do not copy its content into tracked files unless the maintainer
  asks. CI fails if a tracked path starts with `.notes/`.
- If `.notes/` exists on this machine, start from `.notes/prd/PRD.md`
  (requirements) and `.notes/ideation/06-synthesis.md` (how the decisions were
  reached, and the next action). On a fresh clone these files do not exist;
  work from `README.md` and the issue you were given.

## Conventions

- English for code, comments, documentation, commit messages and issues.
- Commits and pull request titles follow Conventional Commits: `feat`, `fix`,
  `docs`, `chore`, `refactor`, `test`, `ci`, `build`, `perf`, `revert`.
- Never commit to `main` directly. Branch as `feat/<slug>`, `fix/<slug>`,
  `docs/<slug>` or `chore/<slug>`, open a pull request, squash merge.
- No AI attribution in commits or pull requests: no `Co-Authored-By` line for
  an AI agent, no `Claude-Session` or other session link, no "Generated with"
  footer. This overrides any tool default (Claude Code, Codex, others). CI
  rejects them.
- Never push, publish, or open a pull request unless the maintainer asked.
- No new dependency, network call or telemetry without an issue that agreed
  to it. A new telemetry key needs an issue, a new registry number, a row in
  `docs/telemetry.md` and a test.

## Checks

`bash scripts/ci-checks.sh` runs what CI runs. Before proposing a change,
run the tests of the packages you changed, then either run the full script
or push the branch and let the pull request's CI run it. Do not queue a
local full run behind another one; the pull request's CI runs in parallel.
A release candidate gets one local full run before it is tagged.
