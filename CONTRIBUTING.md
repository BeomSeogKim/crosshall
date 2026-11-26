# Contributing

Thanks for your interest. The project is at the design stage, so the most
useful contributions right now are issues: use cases, problems you hit when
agents from different vendors work together, and feedback on the direction
described in the README.

## Before you write code

Open an issue first for anything larger than a typo or a small fix, and wait
for a maintainer to agree on the approach. This saves you from building
something that cannot be merged.

## Workflow

The repository is trunk-based.

- `main` is protected. Nobody pushes to it directly; every change arrives
  through a pull request.
- Branch from `main` with a short-lived branch named by purpose:
  `feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `chore/<slug>`.
- Pull requests are squash-merged, so the pull request title becomes the commit
  on `main`.
- History on `main` is linear. Force pushes and branch deletion are blocked.
- Releases are tags on `main` that follow [Semantic Versioning](https://semver.org/).
  The release workflow retains the four archives and their SHA-256 files
  as a tag-run artifact. Attach all eight files to the GitHub release.
  To reproduce a tag build locally, check out the tag and run
  `bash scripts/release.sh <tag> <output-directory>` with the Go version
  in `go.mod`.
- Before a tag, `bash scripts/release-gate.sh --version <tag>` runs the
  recurring gate checks on the candidate commit (reproducible build, release
  dating, migrations, install, setup and uninstall, upgrade and rollback of a
  copy of your store, hook cost against the previous tag, a Claude Code and
  Codex round trip, two devices on one machine against the base tag, and one
  locked full run) and prints a summary table. The link check pairs the
  candidate with the base in both directions and checks that an incompatible
  link version is refused. Checks for the release's new features stay manual,
  and so does row content: the gate checks that a migration
  runs, matches a fresh store's schema and rolls back, while whether it
  changes rows correctly stays with the tier-2 review of the migration.

## Pull requests

- Title: a [Conventional Commit](https://www.conventionalcommits.org/), for
  example `feat: add channel roster` or `fix(cli): reject unknown alias`.
  Allowed types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`,
  `build`, `perf`, `revert`.
- Keep one purpose per pull request.
- CI must pass. You can run the same checks locally:

  ```sh
  bash scripts/ci-checks.sh
  ```

  It needs Go and Python 3; Python runs the installer and terminal tests.
  On Linux, CI also builds the pull request's base and v0.2.0 and runs
  this build's hooks under each one's dispatcher, as after an upgrade in
  place. To run that check locally, build them with
  `go build -tags=testvendorhome` and set `CROSSHALL_E2E_BASE_BINARY` and
  `CROSSHALL_E2E_RELEASE_BINARY`; a build left unset is skipped.
  The race detector runs nightly on main, not on pull requests (it makes
  these tests 14 to 25 times slower). Run it locally when you change
  concurrent code; on Linux it needs cgo:

  ```sh
  CGO_ENABLED=1 go test -race ./internal/dispatch ./internal/appserver ./internal/publish ./internal/store
  ```

- Resolve review conversations before merging.

## Sign your commits

Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org/). Add a
sign-off line to each commit to state that you have the right to submit it:

```sh
git commit -s
```

## Private notes

`.notes/` is ignored by git and is reserved for maintainers' local working
notes. Never add a file under `.notes/` to a commit. CI rejects it.

## Language

Code, comments, documentation, commit messages and issues are in English.

## License

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE), the same license as the project.

## Conduct

Everyone taking part is expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
