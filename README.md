# Cloudflare Stream -- Go Uploader
Inital commit.

## Example Usage:
```sh
read -rs T_apitoken && export T_apitoken   # paste the API token, then press Enter (not echoed or saved in shell history)
./cfsgo --file [file-path] --acctid [account-id] --chunksize 20
```

On Windows, run `.\cfsgo.exe` with the same flags from PowerShell or Windows Terminal.

Every option can be given as a flag or as an environment variable. Variable names are case-sensitive; a flag wins over its variable.

| Flag | Environment variable | Value |
| --- | --- | --- |
| `--acctid` | `T_acctid` | Cloudflare account ID (required) |
| `--apitoken` or `--token` | `T_apitoken` or `T_token` | Cloudflare API token (required) |
| `--file` | `T_file` | Path of the video file to upload (required) |
| `--chunksize` | `T_chunksize` | Chunk size in MB, 5-200 (default 5) |

- Prefer `T_apitoken` for the token. `--token` / `--apitoken` still work, but a token on the command line is visible to other users of the machine (e.g. in `ps`) and can end up in your shell history. In PowerShell 7.1+, `$env:T_apitoken = Read-Host -MaskInput "API token"` sets it without echoing it.
- cfsgo needs an interactive terminal. Chunks that fail with a temporary error (network error, timeout, 5xx) are retried with backoff, up to 8 attempts in a row.
- When the upload finishes, cfsgo prints the video ID and exits with status 0. It exits with 1 if the upload fails and 130 if you cancel it with Ctrl-C.

## Releases
Pushing a `v<number>…` tag (e.g. `v1.0.0`) builds Linux, macOS and Windows binaries (amd64 + arm64) and publishes them, with `SHA256SUMS.txt`, as a GitHub Release:

```sh
git tag v1.0.0 && git push origin v1.0.0
```

Tags containing a `-` (e.g. `v1.0.0-rc1`) are marked as pre-releases. macOS binaries are unsigned; clear the quarantine flag with `xattr -d com.apple.quarantine ./cfsgo` if Gatekeeper blocks them.