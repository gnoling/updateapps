# Lua scripts

Use `source: {type: script}` when no declarative source can describe how an upstream
publishes. A script only resolves the latest version and its download URL; installing
stays declarative, so staging, archive safety and post hooks still apply.

Try the declarative sources first. The scripts in use are the examples to copy:
`eden.yaml` (inline; the link exists only in release-notes markdown) and `flycast.yaml` +
`flycast.lua` (reads an S3 bucket listing).

```yaml
source:
  type: script
  lua: |                      # or: script_file: name.lua   (relative to this YAML)
    local body, status = http.get("https://example.org/builds.json")
    if status ~= 200 then error("listing returned HTTP " .. status) end
    local build = jq(body, '.builds | map(select(.os == "linux")) | max_by(.date)')
    if not build then return nil, "no Linux build yet" end
    return { version = build.id, url = build.url }
```

## What a script returns

| return | meaning |
|---|---|
| `{ version = ..., url = ... }` | Required fields. `version` is compared for equality with what's recorded. |
| optional fields | `display` (shown instead of `version`), `filename` (default: from the URL), `sha256` (hex; the download is verified against it), `headers` (table, sent with the download) |
| `nil, "reason"` | Nothing to install right now. Reported, not a failure, retried next run. |
| `error("...")` or any runtime error | The app fails for this run (exit code 1). Nothing is recorded. |

## API

```
http.get(url, [headers])              -> body, status, headers
http.head(url, [headers])             -> status, headers
http.post(url, body, [headers])       -> body, status, headers
http.post_json(url, table, [headers]) -> decoded response, status
json.decode(s) / json.encode(t)
yaml.decode(s)
jq(value, expr)                       -> first result, or nil     (value: table or JSON string)
jq_all(value, expr)                   -> list of every result
html.select(body, selector, [attr])   -> list of strings: each match's text, or that attribute
gh.api(path)                          -> decoded table (authenticated GitHub REST)
url.resolve(base, ref)                -> absolute URL
url.basename(u)                       -> last path segment, query string dropped
log.info(...) / print(...)            -> the app's output block at -v
log.warn(...)                         -> the app's output block, always
config.name, config.appdir, config.appimagedir, config.home, config.dest
```

- A non-2xx status is a result: check `status`. A transport failure raises an error.
- Response header names are lower-case: `headers.etag`, `headers["last-modified"]`.
- `html.select` works on XML too; tag names match case-insensitively (`"contents > key"`).
- JSON `null` is `nil`. A table with keys exactly `1..n` encodes as a list; any other,
  including `{}`, as an object.

## Sandbox

Scripts get the Lua language (`string`, `table`, `math`), `os.time/date/clock/difftime`
and the API above: no `io`, `require`, `os.execute` or filesystem access, and 60 seconds
per run including HTTP. Requests use the shared client: retries, the GitHub token (sent
only to GitHub), `insecure: true` if set, and `-vv` tracing.
