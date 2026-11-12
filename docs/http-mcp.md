# HTTP MCP server: indexes, keys, and access

`qmd mcp --http` serves several named indexes from one process. Each index is
mounted at its own URL path, and bearer keys decide which paths a client may
reach.

```
qmd mcp --http --addr 127.0.0.1:8422 --server-config ~/.config/qmd/server.yml
```

The server config is resolved from `--server-config`, then `$QMD_SERVER_CONFIG`,
then `$QMD_CONFIG_DIR/server.yml`, then `~/.config/qmd/server.yml`. A service
should set one of the first two, because a default that follows `$HOME` silently
points somewhere else when the unit changes account or scope. The same
resolution applies to `qmd server key` and `qmd server check`.

Two files matter, and they are separate on purpose:

| File | Holds | Read by |
|---|---|---|
| `~/.config/qmd/<index>.yml` | Collections: name, path, glob, context | CLI and server |
| `~/.config/qmd/server.yml` | Mounts (URL path to index), groups, keys | Server only |

## Model

- An **index** is a SQLite file holding documents, the FTS table, and vectors.
  The default index is `index`. Named indexes are selected with `qmd --index <name>`.
- A **collection** is a directory tree inside one index.
- A **mount** is a URL path served by the HTTP server, pointing at one index.
  Several mounts may point at the same index, for example `/mcp` as an alias of
  `/mcp/shared`. Aliases share one database handle and one MCP server. A mount
  path is normalized (leading `/`, no trailing slashes), so one path is mounted
  once; a path the router would rewrite before dispatch (a `.` or `..` segment,
  a doubled slash between segments) is refused when the config loads, as is a
  path that is not its own URL-escaped form, because the router matches the
  escaped request path while the key check matches the decoded one.
- A **group** is a name for a set of mounted paths. It holds no keys.
- A **key** is one record. It has a hash, a label, optional timestamps, and
  exactly one group.

A key that reaches several mounts does so because its group lists several paths.
A key never belongs to two groups.

## Keeping indexes current

`qmd update` reindexes every `<index>.yml` in the config directory, and
`qmd embed` fills vectors for every index the same way. Name an index to narrow
either run; a missing `<name>.yml` is an error, not a silent no-op. Both skip
`server.yml`.

Embeddings are per model, and the vector's dimension is part of the data: an
index holding vectors from model A cannot be queried with model B. `qmd embed`
records the model in `content_vectors` and refuses to run when the configured
`QMD_EMBED_MODEL` differs from what is stored:

    index "personal": holds embeddings from model "jina-v5-small-text-matching" while the configured model is "nomic-embed-text"; run qmd embed --force to rebuild

`--force` clears the targeted indexes' vectors and re-embeds their documents;
with no `--index` that is every index, so narrow it when only one model changed.
Nothing rebuilds automatically, so a model change is a deliberate rebuild.

A reindex unit needs the config directory and two lines:

```ini
Environment=QMD_CONFIG_DIR=/mnt/srv/qmd
ExecStart=/mnt/pool/tools/bin/qmd update
ExecStart=/mnt/pool/tools/bin/qmd embed
```

## The key file

```yaml
servers:
  /mcp: index
  /mcp/personal: personal
  /mcp/work: work

auth:
  public: [/mcp]          # reachable without a key

  groups:                 # paths only
    personal:
      allow: ["/mcp/personal", "/mcp"]
    work:
      allow: ["/mcp/work"]

  keys:                   # one record per key
    - hash: "sha256:ab12..."
      group: personal
      label: laptop
      issued: 2026-11-12T00:00:00Z
      expires: 2027-02-10T00:00:00Z
```

- `hash` is the key's SHA-256, written as `sha256:<64 hex>`.
- `group` must name a group under `auth.groups`.
- `label`, `issued` and `expires` are optional. `expires` absent means the key
  never expires.
- Timestamps are RFC3339 or `YYYY-MM-DD`, which is read as 00:00 UTC.
  `0001-01-01` is refused: it is the zero time, which would read as "never".

The server treats the file as the only record of which keys exist. It rejects a
config that names the same hash twice, and it rejects `auth.groups.*.keys` and
`auth.groups.*.key_hashes`, which are the older format. See
[Converting an old key file](#converting-an-old-key-file).

## Adding an index

1. Create the index config, either by hand or with the collection command:

   ```bash
   qmd --index work collection add ~/work/docs --name docs
   ```

   This writes `~/.config/qmd/work.yml`. Add `db_path: /path/to/work.sqlite` to
   that file if the database should live somewhere other than the default
   resolution order (`db_path`, then `$QMD_DB_DIR`, then `$INDEX_PATH`, then
   `~/.cache/qmd/<name>.sqlite`).

2. Fill the index:

   ```bash
   qmd --index work update
   qmd --index work embed
   ```

   Naming the index limits the run to it, and fails if `work.yml` is missing.
   `qmd update` with no `--index` walks every `<index>.yml` in the config
   directory, which is what a reindex service should run so a new index needs no
   service-file edit.

3. Mount it in `server.yml` and restart the service:

   ```yaml
   servers:
     /mcp: index
     /mcp/shared: index
     /mcp/work: work
   ```

   New mounts are read at startup only. A restart is required.

4. Confirm the mount exists and is gated:

   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8422/mcp/work \
     -H 'Content-Type: application/json' \
     -H 'Accept: application/json, text/event-stream' \
     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
   ```

   `404` means the path is not mounted. `401` means the mount exists and you
   have no key for it. Both are expected before a key is added.

### Worked example: a new document root

```bash
# 1. one collection per root, inside an index named after the purpose
qmd --index work collection add ~/work/design-docs --name design

# 2. build that index (update writes FTS, embed writes vectors)
qmd --index work update
qmd --index work embed

# 3. mount it, then restart the service
#    server.yml: /mcp/work: work

# 4. issue a key that may reach only that mount
qmd server key add work --label laptop --allow /mcp/work

# 5. confirm the config has no dead rules
qmd server check
```

An index holds collections. A mount exposes one index. A second document root
for the same topic is a second collection in the same index, not a new index:

```bash
qmd --index work collection add ~/work/api-docs --name api
qmd --index work update && qmd --index work embed
```

Create a separate index when the data belongs to a different audience, since
access is decided per mount and mounts point at indexes.

## Adding a key

```bash
qmd server key add work --label laptop --allow /mcp/work
```

The command generates 24 random bytes, prints the key once, appends one record
to `auth.keys`, and stores only the hash. The plaintext key is never written to
disk by qmd, so a copy of `server.yml` cannot be replayed as a credential. Put
the key in the client config, not in a note.

The positional argument is the group. A group is a client role, not an index and
not a Unix group. If the group does not exist yet, `key add` creates it with the
paths given by `--allow`.

Flags:

- `--label` records what the key is for. Labels appear in `qmd server key list`.
- `--allow` sets the paths for the group, replacing whatever was there (repeat
  the flag for several paths). Omit it to keep existing paths. A group with no
  paths can authenticate but reach nothing.
- `--expires` sets the expiry: `30d`, `720h`, `2027-03-01`, or an RFC3339
  timestamp. Omit it for a key that never expires. A date in the past is
  refused, because it would issue a key that never works.

Issue one key per client so a single client can be revoked without disturbing
the others.

`key add` records `issued` for you. A record written by hand can carry either
timestamp, or none:

```yaml
  keys:
    - hash: "sha256:ab12..."
      group: work
      label: laptop
      issued: 2026-11-12T00:00:00Z
      expires: 2027-02-10T00:00:00Z
```

## Limiting access

Access is decided in this order, for every request:

1. A path whose escaped form differs from its decoded form (any
   percent-encoding, such as `/mcp%2Fpersonal`) is `404`. The router matches the
   escaped path while these checks use the decoded one; only the literal
   spelling names one mount, so the index that answers is the one the key was
   checked against.
2. `/healthz` is always open, for liveness probes.
3. A path that is not listed under `servers:` is `404`.
4. A path listed under `auth.public` is served with no key.
5. Otherwise the request must carry a live key whose group lists the path under
   `allow`. Anything else is `401` with `WWW-Authenticate: Bearer`.

Consequences worth stating plainly:

- A newly mounted path is unreachable until it is either public or in a group's
  `allow` list. Adding an index does not widen access by default.
- A key's group is the only group consulted. There is no union across groups,
  because a hash may not appear twice.
- An expired key is refused exactly like an unknown key: `401`, with an
  `expired` line in the server log. The response does not say which.
- `public:` is an exact path match. Wildcards are not supported.
- A percent-encoded request path is not a second spelling of a mount. Clients
  must use the mount path exactly as written in `servers:`.
- There is no read-only versus read-write distinction. Every MCP tool qmd
  exposes is read-only today, so a key cannot mutate an index.
- Keys are per user, not per path. To give one client narrower access, give it
  its own group.

`/healthz` and the mount paths are the only HTTP endpoints. The MCP handler
itself is stateless, so there are no session cookies or server-side sessions to
leak.

## Key lifetime

`expires` is evaluated against the server clock at request time. A missing
`expires` means the key never lapses. A key that has reached its expiry is
refused from that instant; there is no grace period.

- Expiry is not deletion. The record stays in the file until it is removed, so
  `key list` still shows it and `server check` reports it until you clean up.
- An expiry in the past makes a key unusable the moment the config is saved.
- `issued` is informational. Nothing enforces an age limit.
- Because the file records both timestamps, key age is auditable: `key list`
  shows the expiry, and `server check` warns at 14 days.

## Rotating and revoking

Keys and `public:` entries reload automatically when `server.yml` changes, and
the server logs `reloaded <path>`. A mount change still needs a restart, and the
log says so when it notices one. Until that restart the router keeps the mounts
it started with, so a path that only exists in the edited file answers `404`.

```bash
qmd server key list                    # fingerprint, group, label, expiry, paths
qmd server key add work --label phone  # new key, printed once
# update the client, confirm it works, then:
qmd server key remove sha256:ab12cd34
```

`key remove` takes a fingerprint prefix, as printed by `key list` and in the
server log. The prefix must match one distinct key; if it matches more, the
command prints the candidates and changes nothing. If the same hash is recorded
more than once, `key remove` deletes every copy of it, including a copy a record
got through an alias or a merge key, which is how a config the loader refuses
gets repaired. Removing the only key of a group leaves the group unusable but
harmless, and `server check` reports it.

## Converting an old key file

Older configs stored keys under each group:

```yaml
auth:
  groups:
    work:
      allow: [/mcp/work]
      keys: ["<plaintext>"]        # not read
      key_hashes:                  # not read
        - hash: "sha256:..."
          label: laptop
```

That layout fails to load, with an error naming the group. Move each entry to
`auth.keys` with its group. A hashed entry keeps its hash; a plaintext entry has
to be hashed first, because there is no place left to keep the plaintext:

```bash
printf %s "$KEY" | sha256sum | awk '{print "sha256:" $1}'
```

```yaml
auth:
  groups:
    work:
      allow: ["/mcp/work"]
  keys:
    - hash: "sha256:..."
      group: work
      label: laptop
```

The plaintext then exists only in the client config that presents it.

## Verifying

```bash
qmd server check
```

It reports: mounts whose index config is missing, `allow` paths that are not
mounted, `public` paths that are not mounted, groups with no keys, groups with
no paths, a config where no key and no public path can reach a mount, keys
expiring within 14 days, keys that have already expired, and keys whose expiry
precedes their issue date.

A config that cannot load is not a finding: `server check` exits nonzero and
prints the loader error. That covers duplicate hashes, a key naming a group that
does not exist, a malformed hash, an unparseable or zero-time timestamp, two
mounts that normalize to the same path, an empty `public:` or `allow:` entry, a
mount at `/healthz`, a mount path with a character an HTTP route cannot carry
(whitespace, control, or braces), a mount path the router would rewrite (a `.`
or `..` segment, or a doubled slash between segments), and a mount path that is
not its own URL-escaped form (any `%`, or another character that arrives
percent-encoded).

To test access without a client:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8422/mcp/work \
  -H 'Authorization: Bearer <key>' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
```

Expected: `200` with the key, `401` without it. The server log records each
keyed or denied request with the key's `sha256:<8>` fingerprint, never the key.

## Filesystem exposure

The key gates the HTTP surface only. The index itself is a SQLite file, and
whoever can read that file reads the private index directly:

- Default mode comes from the service's umask. Set `UMask=0027` in the unit so
  the database is `0640`, and keep the service user in its own group.
- A group-readable database is readable by every member of that group,
  regardless of keys.
- A world-readable database is readable by every local account. Check with
  `ls -l` after the first index build, not before, because SQLite creates the
  file with the umask in force at creation time.
- The `-wal` and `-shm` files carry the same data. They inherit the same mode.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `404` on a path you just added | Mount not loaded, restart the service |
| `401` with a key that worked yesterday | Key removed, or expired, or `server.yml` edited without a valid group entry |
| `401` only from one client | Client sends the key to a path its group does not allow |
| Server refuses to start, "recorded twice" | Same hash in two key records; remove one |
| Server refuses to start, "keys belong in auth.keys" | Keys still sit under `auth.groups.*`; move them to `auth.keys` |
| Server exits on startup, "no servers defined" | `server.yml` has no `servers:` map |
| Server exits on startup, "no such file or directory" | `server.yml` is missing |
| Every path returns `401` including one you expected public | `public:` entries are matched exactly, check the leading slash |
| Slow first search | Embedding model cold start, not auth |
