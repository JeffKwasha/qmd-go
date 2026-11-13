# HTTP MCP server: indexes, keys, and access

`qmd mcp --http` serves several named indexes from one process. Each index is
mounted at its own URL path, and bearer keys decide which paths a client may
reach.

```
qmd mcp --http --addr 127.0.0.1:8422 --server-config ~/.config/qmd/CONFIG.yml
```

The server config is resolved from `--server-config`, then `$QMD_SERVER_CONFIG`,
then `$QMD_CONFIG_DIR/CONFIG.yml`, then
`~/.config/qmd/CONFIG.yml`. A service should set one of the first two,
because a default that follows `$HOME` silently points somewhere else when the
unit changes account or scope. The same resolution applies to `qmd server key`
and `qmd server check`.

Two files matter, and they are separate on purpose:

| File | Holds | Read by |
|---|---|---|
| `~/.config/qmd/<index>.yml` | One index: `db_path`, `embed_model`, collections | CLI and server |
| `~/.config/qmd/CONFIG.yml` | Mounts (URL path to index), groups, keys | Server only |

The server file is upper case so a listing puts it above the indexes: `C` sorts
below every lower-case letter both in byte order and at the primary collation
level. Index discovery skips it by name, and refuses a server config left under
the old name `server.yml` rather than walking it as an index called `server`.
The path is configurable: `--server-config` or `$QMD_SERVER_CONFIG` may point
anywhere, and only the default name is special.

## Model

- An **index** is a SQLite file holding documents, the FTS table, and vectors.
  The default index is `index`. Named indexes are selected with `qmd --index <name>`.
  Read commands resolve the index config before opening a store, so a name with
  no `<name>.yml` cannot report "no collections" while an empty database is
  created behind it. `qmd ls` with no `--index` and no `index.yml` lists every
  index in the directory with its database and collections, because listing is
  its job. `search`, `vsearch`, `query`, `get` and `multi-get` read every index
  when no `--index` is given, and merge by rank. `status` and `cleanup` act on
  one index, so they exit nonzero with a one-line hint naming the choices.
  Writers (`collection add`, `context`) still create the file.
- A **collection** is a directory tree inside one index.
- A **mount** is a URL path served by the HTTP server, pointing at one index.
  Several mounts may point at the same index, for example `/mcp` as an alias of
  `/mcp/shared`. Aliases share one database handle. A mount
  path is normalized (leading `/`, no trailing slashes), so one path is mounted
  once; a path the router would rewrite before dispatch (a `.` or `..` segment,
  a doubled slash between segments) is refused when the config loads, as is a
  path that is not its own URL-escaped form, because the router matches the
  escaped request path while the key check matches the decoded one. A mount must
  name an index that exists: the server refuses to start, and `server check`
  exits nonzero, when `<name>.yml` is absent from the config directory. A mount
  to a missing index would open, and create, an empty database, so it is refused
  instead of served.
- A **group** is a name for a set of mounted paths. It holds no keys.
- A **key** is one record. It has a hash, a label, optional timestamps, and
  exactly one group.

A key that reaches several mounts does so because its group lists several paths.
A key never belongs to two groups.

## Keeping indexes current

`qmd update` reindexes every `<index>.yml` in the config directory, and
`qmd embed` fills vectors for every index the same way. Name an index to narrow
either run; a missing `<name>.yml` is an error, not a silent no-op. Both skip
`CONFIG.yml`.

Embeddings are per model, and the vector's dimension is part of the data: an
index holding vectors from model A cannot be queried with model B. The model an
index needs lives in its own file, because the index is a file and the model is
a property of that file:

```yaml
# private.yml
db_path: /mnt/srv/qmd/private.sqlite
embed_model: embeddinggemma:300m    # what this index's vectors were made with
collections: ...
```

`embed_model` is resolved in this order:

1. `--model <spec>` on `qmd embed`, which also records it in the file.
2. `embed_model` in `<name>.yml`.
3. `$QMD_EMBED_MODEL`.
4. The build's default model.

So the environment variable is the value a new index starts from, not the value
an existing index is stuck with. The first `qmd embed` that runs with no
`embed_model` in the file writes the model it used, so the file describes
itself from then on and re-embedding a month later does not depend on what the
unit's environment said at the time. `--model` is the way to change it:

    qmd --index private embed --force --model embeddinggemma:300m

`qmd embed` compares `embed_model` with what `content_vectors` records and
refuses to run when they disagree:

    index "private": holds embeddings from model "jina-v5-small-text-matching" while embed_model in the index file gives "embeddinggemma:300m"; run qmd embed --force to rebuild

`--force` clears the targeted indexes' vectors and re-embeds their documents;
with no `--index` that is every index, so narrow it when only one model changed.
Nothing rebuilds automatically, so a model change is a deliberate rebuild. The
query side reads the same field per index, so `vsearch` and `query` embed the
query with the model the mounted index records. The CLI reads the file per run;
the HTTP server loads it at startup, so a changed `embed_model` reaches it on
the next restart.

A reindex unit needs the config directory and two lines:

```ini
Environment=QMD_CONFIG_DIR=/mnt/srv/qmd
Environment=QMD_EMBED_MODEL=embeddinggemma:300m   # seed for a new index only
ExecStart=/mnt/pool/tools/bin/qmd update
ExecStart=/mnt/pool/tools/bin/qmd embed
```

## What one request searches

A mount names one index, except a catchall mount, written `all`:

```yaml
servers:
  /mcp: all           # everything this caller may reach
  /mcp/pub: public    # this index only
  /mcp/private: private

auth:
  public: [/mcp/pub]
  groups:
    hermes-private:
      allow: [/mcp, /mcp/private]
```

- `all` answers from every index the caller may reach: the indexes of the mounts
  in `auth.public`, plus the indexes behind the paths its key's group allows.
- A mount naming an index answers from that index only, so `/mcp/private` never
  returns a `/mcp/pub` result.

`all` is a keyword, not a pattern. An index is covered because a mount exposing
it is granted to the caller, not because its name matches anything; a new index
joins the catchall as soon as some mount shows it to that caller. There is no
name matching, so `/mcp: pub*` means an index literally called `pub*`.

Status codes:

| situation | answer |
|---|---|
| path not mounted | `404` |
| missing, unknown or expired key on a keyed mount | `401`, and the `WWW-Authenticate` challenge says `key expired` when that is the reason |
| credential accepted for the mount, but no index behind it is readable with it | `403` |

A catchall with nothing reachable is therefore `403`, never an empty `200`: an
empty answer must mean "nothing matched", not "nothing was allowed". A read
also refuses an index whose stored vectors came from another model, with the
same message `qmd embed` gives, rather than returning an empty list. A catchall
only has what the other mounts grant — with `public: [/mcp]` alone and no public
index mount, an anonymous client gets `403`, so give it one (`/mcp/pub: public`)
or expect a key. A config in which every mount is `all` is refused at startup.

Results merge by **rank**, never by score:

- BM25 scores depend on the corpus of the index that produced them, so a 12 in
  one index and a 7 in another say nothing about their order.
- Vector scores depend on the embedding model, and two indexes may use different
  models. The query is embedded once per index, with that index's own
  `embed_model`, and the hit lists are fused afterwards.

Fusion is reciprocal rank fusion (`k=60`). The top result is reported as 100%
and the rest relative to it; that number is a ranking, not a confidence. Every
result carries its `index` name, and the CLI prefixes it (`public:qmd://...`)
whenever more than one index answered. A single-index answer keeps its own
scores, so nothing changes for a single-index setup.

The CLI does the same thing by directory: with no `--index` and no `index.yml`,
`search`, `vsearch`, `query`, `get` and `multi-get` read every index there, and
each printed path carries its index (`public:qmd://pub/doc.md`). `qmd get`
accepts that form, so a federated hit can be fetched without guessing.
`--csv` carries the index in its own column, and `--files` prefixes the path.

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

3. Mount it in `CONFIG.yml` and restart the service:

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
#    CONFIG.yml: /mcp/work: work

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
disk by qmd, so a copy of `CONFIG.yml` cannot be replayed as a
credential. Put
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
- An expired key is refused with `401` and an `expired` line in the server log.
  The challenge adds `error_description="key expired"`, so a client can tell it
  from an unknown key without reading the log.
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

Keys and `public:` entries reload automatically when `CONFIG.yml` changes, and
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

It reports findings in two grades. A **problem** means a mount or a key cannot
work, and makes the command exit nonzero: `allow` paths that are not mounted,
`public` paths that are not mounted, a group that holds keys but allows no
paths, a config where no key and no public path can reach a mount, keys that
have already expired, and keys whose expiry precedes their issue date. A
**warning** is housekeeping and leaves the exit status at 0: groups with no
keys, and keys expiring within 14 days.

A config that cannot be served is not a finding: `server check` exits nonzero
and prints the error instead of a report. That covers a mount naming an index
with no `<name>.yml`, duplicate hashes, a key naming a group that
does not exist, a malformed hash, an unparseable or zero-time timestamp, two
mounts that normalize to the same path, an empty `public:` or `allow:` entry, a
mount at `/healthz`, a mount path with a character an HTTP route cannot carry
(whitespace inside the path, control characters, or braces), a mount path the router would rewrite (a `.`
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
| `401` with a key that worked yesterday | Key removed, or expired, or `CONFIG.yml` edited without a valid group entry |
| `401` only from one client | Client sends the key to a path its group does not allow |
| Server refuses to start, "recorded twice" | Same hash in two key records; remove one |
| Server refuses to start, "keys belong in auth.keys" | Keys still sit under `auth.groups.*`; move them to `auth.keys` |
| Server exits on startup, "no servers defined" | `CONFIG.yml` has no `servers:` map |
| Server exits on startup, a mount "points at index ... which has no config" | The mount names an index with no `<name>.yml`; fix the name or create the index |
| Server exits on startup, "no such file or directory" | `CONFIG.yml` is missing |
| `no index.yml in ...; choose --index a or --index b` | Ran without `--index` in a directory whose indexes have other names; the default is `index` |
| `db_path "rel.sqlite" is relative` | Pin an absolute path; a relative one is one database per working directory |
| `CONFIG.yml is the server config, not an index` | A read or write named the server file as an index |
| Every path returns `401` including one you expected public | `public:` entries are matched exactly, check the leading slash |
| Slow first search | Embedding model cold start, not auth |
