# crosshall - agent guide

Rules for AI coding agents working in this repository. `CLAUDE.md` is a symlink
to this file; edit `AGENTS.md` only.

## Status

crosshall is released; the tags, `README.md` and `CHANGELOG.md` say which
version is current. `crosshall setup` installs the adapter hooks into Claude
Code, Codex CLI and Antigravity CLI; `README.md` and `docs/adapters.md` list
the builds and platforms each feature is verified on. The project was called
`sotto` while it was being designed; older notes may still use that name.

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
run the tests of the packages you changed. The pull request's CI is the full
run; do not run the full script locally for an ordinary pull request. A
release candidate gets one local full run before it is tagged.
