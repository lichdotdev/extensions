# lich extension catalog

The list of extensions that [lich](https://github.com/lichdotdev/lich)'s Settings and its site show. The build
reads every repository in `extensions.json`, checks its latest release and writes `catalog.json` and `icons/`,
served from `https://raw.githubusercontent.com/lichdotdev/extensions/main/catalog.json`. The format is the
`extension.Catalog` type in [`extension/catalog.go`](https://github.com/lichdotdev/lich/blob/main/extension/catalog.go),
and what an extension's author needs to know is
[Listing in the catalog](https://github.com/lichdotdev/lich/blob/main/docs/extensions.md#listing-in-the-catalog).

## Getting listed

1. Publish a release the way lich installs it: one `<id>_<goos>_<goarch>.tar.gz` per platform and
   `checksums.txt`, tagged `MAJOR.MINOR.PATCH`. The
   [extension template](https://github.com/lichdotdev/extension-template) does this for you.
2. Check every archive: `lich extension check dist/<id>_linux_amd64.tar.gz`. The command ships in the next lich
   release.
3. Optionally add `listing/listing.json` to your repository with one or two categories, a cover and screenshots.
4. Open a pull request adding `"<owner>/<repo>"` to `extensions.json`. Its check runs on your latest release and
   comments with what fails; push to the pull request to run it again after a new release. A maintainer approves
   the first listing, and every later release is picked up within an hour.

You can run the same check yourself: `go run ./cmd/catalog check <owner>/<repo>`.

## What is checked

- The repository is public, not a fork and not archived.
- Its latest release (neither a draft nor a pre-release) is tagged `MAJOR.MINOR.PATCH`, with or without `v`.
- `checksums.txt` names every archive, and every archive matches its sha256 and passes `lich extension check`.
  A platform without an archive is listed as unavailable there.
- Every manifest has the archive's id and the tag's version, and sets a `description` of at most 160 characters.
- The id is not listed from another repository, and an id starting with `lich` is `lichdotdev`'s.
- `listing/listing.json`, read at the commit the tag points to: known categories, and a cover and screenshots
  within the format, size and pixel limits. A repository without one is listed under `tools`, with a warning.

The name and description are translated from `manifest.name` and `manifest.description` in each
`locales/<tag>/extension.json` of the archive. The first surface's `icon` is the entry's glyph, and its
`iconFile` is copied to `icons/<id>.png`.

## What the catalog holds to

- `releases.lock.json` records each `<id>@<version>` the first time the build sees it: the commit and every
  archive's sha256. That record never changes. A release replaced afterwards keeps the extension at its last good
  entry in `catalog.json`, with the reason in `held`.
- `identities.json` records the repository and GitHub id each id was first listed from. A repository whose GitHub
  id changed (deleted and created again, or another repository claiming the id) is held the same way.
- A release that disappears, or a new one that fails any check, holds the last good entry too. A repository never
  listed is left out until it passes.
- The build fails, and commits nothing, when GitHub itself does not answer; the next run retries.

## Maintainer tasks

**Confirm an identity.** A held entry saying its GitHub id changed waits for you. Make sure the repository is the
same publisher's (a recreated repository, or a transfer they asked for), then set the id's `repoId` (and `repo`,
if it moved) in `identities.json` to the new values and merge. The next build lists it again.

**Revoke a repository.** For malware, deception or infringement, add it to `revoked.json`:

```json
[{ "repo": "https://github.com/<owner>/<repo>", "repoId": 123456, "reason": "Ships a credential stealer" }]
```

`repoId` is the one in `catalog.json` or `identities.json`. The entry leaves `extensions`, the revocation is
copied into the catalog's `revoked`, and every lich that installed it stops it when it next reads the catalog.
Remove the repository from `extensions.json` in the same pull request.

**Add a category.** Add it to `categories.json` with its name in every language lich ships.

## Development

```sh
go vet ./... && go test ./...
```

`.github/workflows/build.yml` builds every hour, on a push to `main` and on demand, and commits what changed.
`.github/workflows/pr.yml` checks the repositories a pull request adds. `go.mod` replaces the lich SDK with a
local checkout until the SDK release that has `catalog.go` is tagged; until then neither workflow builds.

## License

AGPL-3.0, as lich.
