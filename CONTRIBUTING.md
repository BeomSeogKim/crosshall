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
