# Cloudflare Stream -- Go Uploader
Inital commit.

## Example Usage:
```bash
read -rs T_apitoken && export T_apitoken   # bash/zsh: prompts for the token without echoing it.
./cfsgo --file [file-path] --acctid [account-id] --chunksize 20
```

On Windows, set the token with `$env:T_apitoken = Read-Host -MaskInput` (PowerShell 7.1+), then run `.\cfsgo.exe` with the same flags from PowerShell or Windows Terminal.

- Chunk Size should be between 5-200 (MB)
- Pass the API token in the `T_apitoken` environment variable. `--token [api-token]` still works, but command-line arguments show up in `ps` and shell history. The `--token=[api-token]` form is rejected.
- Every option can be set with a `T_` environment variable: `T_apitoken`, `T_acctid`, `T_file`, `T_chunksize`.
- A failed request is retried up to 15 times in a row with exponential backoff (1s, doubling up to 60s; about 10 minutes in all); a failed chunk is resent from the server's offset, and the count starts over whenever the server has received more of the file. A chunk that stops sending for a minute counts as failed. Errors that retrying cannot fix, such as a bad token (401/403), an upload that is too large (413) or an untrusted TLS certificate, stop the upload at once.
- Press Ctrl-C to cancel. The tool exits with 0 when the upload completes, 1 when it fails, 130 when cancelled with Ctrl-C, and 128 + the signal number on SIGTERM (143) or SIGHUP (129). A failed or cancelled run prints the partial upload's URL so it can be found and deleted.

## Releases
A release builds Linux, macOS and Windows binaries (amd64 + arm64) and publishes them, with `SHA256SUMS.txt`, as a GitHub Release. Start one either way:

- **From GitHub:** Actions → Release → Run workflow on `main` with a version such as `v1.0.0`. The workflow builds `main`, then creates the tag and the release (it refuses a version whose tag already exists).
- **From git:** push a `v<number>…` tag:

  ```sh
  git tag v1.0.0 && git push origin v1.0.0
  ```

Versions containing a `-` (e.g. `v1.0.0-rc1`) are marked as pre-releases. macOS binaries are unsigned; clear the quarantine flag with `xattr -d com.apple.quarantine ./cfsgo` if Gatekeeper blocks them.