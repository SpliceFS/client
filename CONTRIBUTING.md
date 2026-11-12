# Contributing to SpliceFS

Thank you for helping. This page describes how changes get in and the standards they are
held to. Participation is covered by the [code of conduct](CODE_OF_CONDUCT.md).

## Before you start

- **Bugs:** open an issue with the version, what you did, what you expected and what happened.
- **Features and behavior changes:** open an issue first and describe the problem you want
  solved. The [design document](docs/design.md) is the specification; a change in behavior
  starts as a change to that document. The HTTP API and the consistency rules are specified in
  the [design document of the server repository](https://github.com/SpliceFS/server/blob/main/docs/design.md).
- **Security problems:** do not open an issue. Follow the [security policy](SECURITY.md).

## Development setup

You need Go 1.26 or newer and `make`. Linting uses
[golangci-lint](https://golangci-lint.run) version 2.

```
make build        # build splicefs into ./bin
make check        # format check, vet, tests with the race detector, coverage gate
make lint         # golangci-lint, the same configuration CI uses
make fmt          # rewrite files with gofmt
```

`make check` and `make lint` are exactly what CI runs. If both pass locally, CI will pass.

## Branches

SpliceFS uses the gitflow branching model.

- `main` holds released code only. Every commit on it is a release and carries a version tag.
- `develop` is where work comes together. It is the default branch, and pull requests target it.
- `feature/<name>` branches start from `develop` and merge back into it by pull request.
- `release/<version>` branches start from `develop` when a release is prepared. They take only
  fixes and the changelog, and merge into `main` and back into `develop`.
- `hotfix/<version>` branches start from `main` for urgent fixes and merge into `main` and
  `develop`.

`main` and `develop` are protected: no direct pushes, no force pushes, no deletion, and CI
must pass before a merge.

## Standards

### Code

- Idiomatic Go: [Effective Go](https://go.dev/doc/effective_go) and the
  [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) apply.
- `gofmt` formatted, `go vet` clean, `golangci-lint` clean. No `//nolint` without a reason
  on the same line.
- Errors are returned, wrapped with context (`fmt.Errorf("open share %q: %w", name, err)`),
  and handled once. No panics outside of programmer errors at start-up.
- Every blocking call takes a `context.Context`. Nothing blocks forever.
- No global mutable state. Dependencies (clock, filesystem, HTTP client) are passed in, so
  that every package can be tested without the network, the real clock or root.
- Exported identifiers have doc comments that tell a caller what the thing is for and what
  it guarantees. Packages have a package comment.
- **Comments inside code explain why, not what.** The code already says what it does. A
  comment earns its place by recording a reason the code cannot show: the constraint being
  respected, the attack being prevented, the alternative that was rejected, the surprising
  behavior of something it calls. Delete comments that restate the line below them.
- **SOLID and clean code.** Each package and type has one reason to change. Packages depend
  on small interfaces they define themselves, not on concrete types from other packages,
  so behavior is extended by adding an implementation and not by editing callers. Functions
  are short, do one thing, and are named for it. No flag arguments, no duplicated logic, no
  dead code, no commented-out code.
- Dependencies are added sparingly. A new module in `go.mod` needs a sentence of
  justification in the pull request.
- The server lives in its [own repository](https://github.com/SpliceFS/server). Neither program
  imports the other. What both need (the JSON types of the HTTP API and the size and duration
  types) lives in the [common](https://github.com/SpliceFS/common) module. A change there is
  released first, and then the version in `go.mod` is raised here.

### Tests

- **Statement coverage is 100%, enforced by CI.** A change that lowers it does not merge.
  Code that cannot be reached by a test is a design problem; restructure it so it can.
- Tests are table-driven where there are several cases, use only the standard `testing`
  package, and run with the race detector.
- Tests do not sleep to wait for something and do not depend on wall-clock time. Use an
  injected clock.
- A bug fix comes with a test that fails without the fix.
- Code that parses input from a user or a remote system gets a fuzz test (`FuzzXxx`) that checks
  properties such as no panic, invalid input changing nothing and entries staying inside their
  root. Run them with `make fuzz`, or longer with `FUZZTIME=1m make fuzz`. An input that found a
  bug is added to the table tests of the package.

### Commits and pull requests

- One logical change per pull request. Keep refactoring separate from behavior changes.
- Commit subjects are imperative and at most 72 characters ("Add block scheduler", not
  "Added" or "Adds"). The body explains why, not what.
- Update [CHANGELOG.md](CHANGELOG.md) under **Unreleased** for anything a user would notice.
- Update the documents in `docs/` in the same pull request as the behavior they describe.
- A pull request targets `develop` and needs passing CI. Approval by a second maintainer
  becomes a requirement as soon as the project has one.

## What CI checks

| Workflow | What it does |
|---|---|
| CI | format, vet, tests with the race detector on the two supported Go releases, the 100% coverage gate, a build, and `golangci-lint` (which includes the `gosec` security rules) |
| Security | `govulncheck` for known vulnerabilities in dependencies and the standard library, CodeQL static analysis, and a dependency review on pull requests. Also runs weekly. |
| SonarQube | quality and security rating on SonarQube Cloud. Skipped until a maintainer has set it up. |

One-time SonarQube setup, by a maintainer: import the repository on
[sonarcloud.io](https://sonarcloud.io), turn off its "Automatic Analysis", check that the
organization and project keys match `sonar-project.properties`, and add a token as the
repository secret `SONAR_TOKEN`.

One-time Codecov setup, by the organization owner: sign in at [codecov.io](https://codecov.io)
with GitHub and enable the repository. No secret is needed, because the upload uses OIDC.

The Scorecard workflow needs no setup.

## Versioning and releases

SpliceFS follows [Semantic Versioning](https://semver.org).

- **Patch** (`0.3.1`): bug fixes only.
- **Minor** (`0.4.0`): new features. Before 1.0.0, a minor release may also change the API
  or the configuration format; the changelog says so under a **Changed** heading.
- **Major** (`1.0.0` and later): incompatible changes to the HTTP API, the configuration
  format or the command line.

The HTTP API has its own version in the URL (`/v1/`). A server keeps serving an API version
for as long as a supported client release uses it.

A release is cut by a maintainer:

1. Start `release/X.Y.Z` from `develop`.
2. Move the **Unreleased** entries in `CHANGELOG.md` under a new version heading with the date
   and commit as `Release vX.Y.Z`.
3. Merge the branch into `main` by pull request, with a merge commit.
4. Tag that merge commit `vX.Y.Z` and push the tag. The version printed by `--version` comes
   from the tag at build time, and the tag starts the publish job.
5. Merge `main` back into `develop`.

## License

By contributing you agree that your contribution is licensed under the [MIT license](LICENSE).
