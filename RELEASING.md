# Releasing FirebirdREST

## Steps

1. In `CHANGELOG.md`, move the entries under `## [Unreleased]` to a new section
   `## [x.y.z] - YYYY-MM-DD` (with a one-line summary), and add the compare links at the bottom.
   This section becomes the release notes.
2. Commit and push to `main`, and wait for CI.
3. Run **Actions → Release → Run workflow** with the version (`x.y.z`, or `x.y.z-rc1` for a
   pre-release). Alternatively push an annotated tag `vx.y.z`.

The workflow runs the full test suite on Firebird 3, 4, 5 and 6, then:

- tags the commit (when started from the Actions page);
- builds the binaries for Linux, macOS, Windows and FreeBSD, and `.deb` and `.rpm` packages for
  Linux amd64 and arm64 (`packaging/` holds their systemd unit, configuration and scripts), and
  publishes the GitHub release with the CHANGELOG section as its notes (replacing any text of a
  release drafted on GitHub);
- builds and pushes the multi-arch Docker image (`x.y.z`, `x.y`, and `latest` for non-pre-releases);
- publishes the optional packages below, when they are set up.

Don't create the release on GitHub's Releases page first: let the workflow create it.

## Optional publishing targets

To try the packages before a release: `goreleaser release --snapshot --clean --skip=publish`
writes them to `dist/`.

Each target turns on when its settings exist (**Settings → Secrets and variables → Actions**), and
is skipped otherwise.

### Docker Hub

The image is always pushed to `ghcr.io/mariuz/firebirdrest`. To push `docker.io/<user>/firebirdrest`
too:

1. On Docker Hub, create the repository `firebirdrest` and an access token with *Read & Write*
   permission (Account settings → Personal access tokens).
2. Add a repository **variable** `DOCKERHUB_USERNAME` with your Docker Hub user name.
3. Add a repository **secret** `DOCKERHUB_TOKEN` with the token.

### Homebrew and Scoop

GoReleaser writes a Homebrew cask (macOS and Linux) and a Scoop manifest (Windows) on every
release. To publish them:

1. Create two public repositories: `mariuz/homebrew-tap` and `mariuz/scoop-bucket` (each with a
   README so they are not empty).
2. Create a fine-grained personal access token with *Contents: Read and write* on those two
   repositories.
3. Add it as the repository secret `TAP_GITHUB_TOKEN`.

From the next release on, users can install with:

```sh
brew install --cask mariuz/tap/firebirdrest
scoop bucket add mariuz https://github.com/mariuz/scoop-bucket
scoop install firebirdrest
```

Add these to the install instructions (README and `docs/index.md`) once the first package is
published.
