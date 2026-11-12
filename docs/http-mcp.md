# HTTP MCP server: indexes, keys, and access

`qmd mcp --http` serves several named indexes from one process. Each index is
mounted at its own URL path, and bearer keys decide which paths a client may
reach.

```
qmd mcp --http --addr 127.0.0.1:8422 --server-config ~/.config/qmd/server.yml
```

Two files matter, and they are separate on purpose:

| File | Holds | Read by |
|---|---|---|
| `~/.config/qmd/<index>.yml` | Collections: name, path, glob, context | CLI and server |
| `~/.config/qmd/server.yml` | Mounts (URL path to index) and auth groups | Server only |

## Model

- An **index** is a SQLite file holding documents, the FTS table, and vectors.
  The default index is `index`. Named indexes are selected with `qmd --index <name>`.
- A **collection** is a directory tree inside one index.
- A **mount** is a URL path served by the HTTP server, pointing at one index.
  Several mounts may point at the same index, for example `/mcp` as an alias of
  `/mcp/shared`. Aliases share one database handle and one MCP server.
- A **key** belongs to one group. A group holds a list of keys and a list of
  paths those keys may use.

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

# 5. verify 200 with the key and 401 without it
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

The command generates 24 random bytes, prints the key once, and stores only
`sha256:<hex>` in `server.yml`. The plaintext key is never written to disk by
qmd, so a copy of `server.yml` cannot be replayed as a credential. Put the key
in the client config, not in a note.

The positional argument is the group name. A group is a client role: a named
bag of keys plus the paths those keys may use. It is not an index, and it is not
a Unix group. One index can be reachable through several groups, and one group
can reach several mounts.

Flags:

- `--label` records what the key is for. Labels appear in `qmd server key list`.
- `--allow` sets the paths for the group, replacing whatever was there (repeat
  the flag for several paths). Omit it to keep existing paths. A group with no
  paths can authenticate but reach nothing.

Issue one key per client so a single client can be revoked without disturbing
the others.

Only the hash is stored, so a key cannot be recovered from `server.yml`:

```yaml
groups:
  work:
    allow: ["/mcp/work"]
    key_hashes:
      - hash: "sha256:<hex>"
        label: laptop
```

A bare `sha256:<hex>` string instead of the mapping is also accepted.

## Limiting access

Access is decided in this order, for every request:

1. `/healthz` is always open, for liveness probes.
2. A path that is not listed under `servers:` is `404`.
3. A path listed under `auth.public` is served with no key.
4. Otherwise the request must carry a valid key whose group lists the path
   under `allow`. Anything else is `401` with `WWW-Authenticate: Bearer`.

Consequences worth stating plainly:

- A newly mounted path is unreachable until it is either public or in a group's
  `allow` list. Adding an index does not widen access by default.
- A key that appears in two groups is allowed if either group allows the path.
  `qmd server check` reports duplicate keys, which are almost always a mistake.
- `public:` is an exact path match. Wildcards are not supported.
- There is no read-only versus read-write distinction. Every MCP tool qmd
  exposes is read-only today, so a key cannot mutate an index.
- Keys are per user, not per path. To give one client narrower access, give it
  its own group.

`/healthz` and the mount paths are the only HTTP endpoints. The MCP handler
itself is stateless, so there are no session cookies or server-side sessions to
leak.

## Rotating and revoking

Keys and `public:` entries reload automatically when `server.yml` changes, and
the server logs `reloaded <path>`. A mount change still needs a restart, and the
log says so when it notices one.

```bash
qmd server key list                      # fingerprints, groups, paths
qmd server key add work --label laptop   # new key, printed once
# update the client, confirm it works, then:
qmd server key remove sha256:ab12cd34 --group work
```

`key remove` takes a fingerprint prefix, as printed by `key list` and in the
server log. Removing the only key of a group leaves the group unusable but
harmless.

Plaintext `keys:` entries are still read for compatibility, so an older config
keeps working. `qmd server check` warns when it finds one, and `key add` never
writes one.

## Verifying

```bash
qmd server check
```

It reports: mounts whose index config is missing, `allow` paths that are not
mounted, groups with no keys, duplicate keys across groups, and plaintext keys
that should be rotated.

To test access without a client:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8422/mcp/work \
  -H 'Authorization: Bearer <key>' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
```

Expected: `200` with the key, `401` without it. The server log records every
decision with the key's `sha256:<8>` fingerprint, never the key.

## Key lifetime

Keys do not expire. There is no TTL and no scheduled revocation, so a key that
is issued and forgotten stays valid until someone removes it. Two consequences:

- Rotation is the only revocation. Keep one key per client so removing one does
  not disturb the others.
- Nothing in the config records when a key was issued, so age is not auditable
  from the file. The server log shows use by fingerprint, which is the closest
  thing to a last-seen record.

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
| `401` with a key that worked yesterday | Key removed, or `server.yml` edited without a valid group entry |
| `401` only from one client | Client sends the key to a path its group does not allow |
| `500` on startup, "no servers defined" | `server.yml` missing or has an empty `servers:` map |
| Every path returns `401` including one you expected public | `public:` entries are matched exactly, check the leading slash |
| Slow first search | Embedding model cold start, not auth |
