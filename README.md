# Cloudflare Stream -- Go Uploader
Inital commit.

## Example Usage:
./cfsgo --file [file-path] --token [api-token] --acctid [accout-id] --chunksize 20

On Windows, run `.\cfsgo.exe` with the same flags from PowerShell or Windows Terminal.

- Chunk Size should be between 5-200 (MB)
- 

## Releases
A release builds Linux, macOS and Windows binaries (amd64 + arm64) and publishes them, with `SHA256SUMS.txt`, as a GitHub Release. Start one either way:

- **From GitHub:** Actions → Release → Run workflow on `main` with a version such as `v1.0.0`. The workflow builds `main`, then creates the tag and the release (it refuses a version whose tag already exists).
- **From git:** push a `v<number>…` tag:

  ```sh
  git tag v1.0.0 && git push origin v1.0.0
  ```

Versions containing a `-` (e.g. `v1.0.0-rc1`) are marked as pre-releases. macOS binaries are unsigned; clear the quarantine flag with `xattr -d com.apple.quarantine ./cfsgo` if Gatekeeper blocks them.