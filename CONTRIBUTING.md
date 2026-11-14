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
[golangci-lint](https://golangci-lint.run) version 2 and [ShellCheck](https://www.shellcheck.net).

```
make build        # build splicefs into ./bin
make check        # format check, vet, tests with the race detector, coverage gate
make compat-linux # the real client against released servers (Docker; make compat on Linux as root)
make lint         # golangci-lint (the configuration CI uses), then ShellCheck with every optional check on
make fmt          # rewrite files with gofmt
```

`make check` is what CI runs, and CI runs golangci-lint the way `make lint` does. `make lint` also
runs ShellCheck, which CI runs as its own job.

## Branches and pull requests

There is one long-lived branch, `main`. It is protected: no direct pushes, no force pushes, no
deletion, and the checks must pass before a merge.

Every change is a pull request into `main` from its own branch. Name the branch
`feature/<name>`, `bug/<name>`, or anything else that describes the change.

### Change scope

The description of a pull request must contain exactly one line that states the scope of the
change:

```
change_scope: revision
```

| Value | Meaning | Version |
| --- | --- | --- |
| `revision` | a fix or a change that keeps every client and server working together | `X.Y.Z` becomes `X.Y.(Z+1)` |
| `minor` | a new feature, or anything that changes how client and server talk | `X.Y.Z` becomes `X.(Y+1).0` |
| `major` | never merged | a new major version is a new repository named `<name>_v<major>` |

The pull request template has the line. A missing or invalid line fails the check, whatever the
tests say.

### The version

The release bot reads the version of `main`, raises it by the scope, and adds one commit to your
branch that writes it to the file `VERSION`. Do not edit `VERSION` by hand. The one exception is
a pull request from a fork, where the bot cannot push: set `VERSION` to the value the failing
check names.

### Keeping the branch current

A branch must sit on the tip of `main`. When `main` moves, the bot rebuilds every open pull
request on the new tip and sets its version again, so expect force-pushes to your branch. Run
`git pull --rebase` (or reset to the remote branch) before you add commits. If the rebuild hits
a conflict, the bot leaves the branch as it is and says so in a comment. Rebase it by hand and
push.

### Merging

A pull request merges with one merge commit. Squash and fast-forward merges are off.

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
- Write what a user would notice in the "Change log" section of the pull request, or one line
  that starts with `Internal:` when there is nothing. The release bot writes it into
  [CHANGELOG.md](CHANGELOG.md) with the new version; do not edit that file yourself.
- Update the documents in `docs/` in the same pull request as the behavior they describe.
- A pull request targets `main` and needs passing CI. Approval by a second maintainer
  becomes a requirement as soon as the project has one.

## What CI checks

| Workflow | What it does |
|---|---|
| CI | format, vet, tests with the race detector on the two supported Go releases, the 100% coverage gate, a build, and `golangci-lint` (which includes the `gosec` security rules) |
| Rules | on pull requests: checks the `change_scope` line and the `VERSION` file the bot sets |
| Main | on every push to `main`: tags the new version and rebuilds the open pull requests |
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

Versions are `0.x.y` for now, with no suffixes. The promise about compatibility:

- Below 1.0.0 nothing is promised, even between a client and a server of the same version.
- From 1.0.0 on, with the same first number, a client works with every server whose second
  number is equal or higher. The third number never matters, barring a bug; a bug that
  affects it is fixed by raising the second number on both sides.

Every merge into `main` is a release. The merge is tagged `v<VERSION>` automatically, which
starts the Release workflow. Nobody creates tags or release branches by hand. One change, one
version. The version printed by `--version` comes from the tag at build time.

The HTTP API has its own version in the URL (`/v1/`). A server keeps serving an API version
for as long as a supported client release uses it.

The Release workflow builds the deb, rpm and apk packages for both CPU types, signs the apt, rpm
and Alpine repositories, installs every package and every repository in clean containers of each
supported system, and only when all of that passes publishes the repositories to
`packages.splicefs.org/client` (older versions stay in them). It then creates the GitHub release
for the tag, with the packages, `SHA256SUMS` and its signature `SHA256SUMS.asc`, and the
merged pull request as the notes.

## License

By contributing you agree that your contribution is licensed under the [MIT license](LICENSE).
