# Cloudflare Stream -- Go Uploader
Inital commit.

## Example Usage:
./cfsgo --file [file-path] --token [api-token] --acctid [accout-id] --chunksize 20

On Windows, run `.\cfsgo.exe` with the same flags from PowerShell or Windows Terminal.

- Chunk Size should be between 5-200 (MB)
- 

## Releases
A release builds Linux, macOS and Windows binaries (amd64 + arm64) and publishes them, with `SHA256SUMS.txt`, as a GitHub Release. Start one either way:

- **From GitHub:** Actions → Release → Run workflow on `main` with a version such as `v1.0.0` or `v1.0.0-rc.1`. The workflow builds `main`, tags the commit it built, then publishes the release. A version already tagged on a different commit is refused before anything is built. If it fails because `main` changed workflow files while it was building, start a new run from the current `main` (re-running the old one reuses the old commit).
- **From git:** push a `v<number>…` tag:

  ```sh
  git tag v1.0.0 && git push origin v1.0.0
  ```

Versions containing a `-` (e.g. `v1.0.0-rc1`) are marked as pre-releases. A release already drafted in the GitHub UI for the version is reused: the binaries are attached and the draft is published. macOS binaries are unsigned; clear the quarantine flag with `xattr -d com.apple.quarantine ./cfsgo` if Gatekeeper blocks them.