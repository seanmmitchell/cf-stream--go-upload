# Cloudflare Stream -- Go Uploader
Inital commit.

## Example Usage:
```sh
read -rs T_apitoken && export T_apitoken   # Prompts for the token without echoing it.
./main --file [file-path] --acctid [account-id] --chunksize 20
```

- Chunk Size should be between 5-200 (MB)
- Pass the API token in the `T_apitoken` environment variable. `--token [api-token]` still works, but command-line arguments show up in `ps` and shell history.
- Every option can be set with a `T_` environment variable: `T_apitoken`, `T_acctid`, `T_file`, `T_chunksize`.
- A failed chunk is retried up to 8 times with exponential backoff (1s, doubling up to 30s), after re-reading the server's offset. Errors that retrying cannot fix, such as a bad token (401/403) or an upload that is too large (413), stop the upload at once.
- Press Ctrl-C to cancel. The tool exits with 0 when the upload completes, 1 when it fails and 130 when cancelled.
