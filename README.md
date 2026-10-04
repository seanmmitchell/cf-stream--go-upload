# Cloudflare Stream -- Go Uploader
Inital commit.

## Example Usage:
./cfsgo --file [file-path] --token [api-token] --acctid [accout-id] --chunksize 20

- Chunk Size should be between 5-200 (MB)
- 

## Releases
Pushing a `v*` tag builds Linux, macOS and Windows binaries (amd64 + arm64) and publishes them, with `SHA256SUMS.txt`, as a GitHub Release:

```sh
git tag v1.0.0 && git push origin v1.0.0
```

Tags containing a `-` (e.g. `v1.0.0-rc1`) are marked as pre-releases. macOS binaries are unsigned; clear the quarantine flag with `xattr -d com.apple.quarantine ./cfsgo` if Gatekeeper blocks them.