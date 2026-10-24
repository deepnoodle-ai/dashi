# Releasing

Dashi is a root module plus three tagged sub-modules: `mcp`,
`providers/google/gemini` and `cmd/dashi`. The list lives once, in
`scripts/sync_module_versions.py`, and a test fails if a tracked `go.mod` is
missing from it. Everything below assumes an up-to-date `main` and an
authenticated `gh`.

## 1. Cut the changelog

Rename `## [Unreleased]` in `CHANGELOG.md` to the version being released and
leave a fresh empty `## [Unreleased]` above it. Confirm every merged PR since the
last release has an entry (`git log v<previous>..HEAD --oneline`). Keep each
entry to one to three lines that state the user-visible change.

## 2. Sync the module requirements

```sh
make release-prep VERSION=v0.2.0
```

Every sub-module builds locally through the repository's `go.work`, so a stale
`github.com/deepnoodle-ai/dashi vX.Y.Z` requirement is invisible to tests but
breaks anyone who resolves the tag. The `go.mod` files carry no `replace`
directives, because `go install …@version` refuses a module that has them.
This rewrites every intra-repo requirement, including `cmd/dashi`'s on `mcp`
and `providers/google/gemini`, and pins `go.work`'s replacements to the new
version so the workspace builds before the tag exists. Do not run
`make tidy-all` until after tagging: `go mod tidy` ignores the workspace and
cannot fetch an untagged version.

## 3. Draft the release notes

```sh
make release-notes VERSION=v0.2.0
```

This writes `docs/releases/v0.2.0.md`: `Highlights` (yours to write; a
placeholder until you do), `Pull requests` (squash-merge subjects since the
previous root tag), and `Changelog` (the version's `CHANGELOG.md` section,
verbatim). Write two to four short paragraphs of Highlights: what changed, who
it affects, what to do. Read the merged PR bodies, not only the subjects.
Re-running keeps written Highlights and refreshes the rest.

## 4. Open the release PR

Commit the changelog, the `go.mod` and `go.work` updates, and the notes together
as `chore(release): prepare v0.2.0`. The notes get reviewed alongside the code.

## 5. Tag

After the PR merges, from an updated `main`:

```sh
git tag v0.2.0
make tag-modules VERSION=v0.2.0
git push origin mcp/v0.2.0 providers/google/gemini/v0.2.0 cmd/dashi/v0.2.0
git push origin v0.2.0
```

Push the root tag on its own, last. GitHub starts no workflow for tags pushed
more than three at a time, and the root tag starts the release workflow.

Every tag must point at a commit whose `go.mod` files name the release version.
`tag-modules` tags the sub-modules only, never the root, and re-runs the
requirement check first so a skipped step 2 stops here.

For `v0.1.0` the root tag predates this tooling and sits on an earlier commit.
The sub-module tags go on the commit that set their requirements to `v0.1.0`,
which is fine: they require root `v0.1.0`, which resolves to the existing tag.

## 6. The release workflow publishes

Pushing the root tag runs `.github/workflows/release.yml`. It refuses while the
Highlights in `docs/releases/v0.2.0.md` are a placeholder, runs the tests, and
then GoReleaser (`.goreleaser.yaml`) builds `cmd/dashi` for macOS, Linux, and
Windows, creates the GitHub release with those notes and the binaries, and
updates the `dashi` formula in `deepnoodle-ai/homebrew-tap`, so
`brew install deepnoodle-ai/tap/dashi` gets the new version. The workflow needs
the `TAP_GITHUB_TOKEN` secret, a token that can push to the tap.

Watch the run with `gh run watch`, and rerun a failed run with `gh run rerun`.

`make release-notes-test` covers the tooling; `make check` runs it with gofmt,
vet and tests in every module.
