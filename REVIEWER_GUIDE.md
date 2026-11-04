# Interviewer guide — catalog GraphQL review

Do not send this file to the candidate. Give them `README.md`, `main.go`, and `store.go` only.

The sample is a pull request with planted defects. It is written to compile. It is not safe to run against a shared database.

## How to score the review

A senior review is prioritized and specific. Count a finding only if they name the user-visible or production consequence and a concrete fix. "Add error handling" without saying which error is swallowed does not count.

| Weight | What a strong review does |
| --- | --- |
| High | Leads with authorization, query injection, and the publish/read field mismatch. These change who can write data and what data comes back. |
| High | Calls out the detached request context, the data race, and unbounded Mongo reads as availability bugs, not style. |
| Medium | Covers the slug race, non-atomic publish, stale cache, swallowed Mongo errors, and secrets in logs. |
| Medium | Names the test that would have caught each correctness bug. |
| Low | HTTP server timeouts, GraphiQL, CORS, health check, predictable ids. Fine as follow-ups. They should not dominate the review. |

Weak signals:

- A list of naming and comment nits, with the race or the auth check missing.
- Rewriting the service in the review instead of saying what must change.
- Flagging HTTP 200 on GraphQL field errors. That is what the spec says. The problem is the error text and the cases that are not GraphQL errors (bad JSON, auth).
- Flagging the `go func(id string)` parameter as a loop-capture bug. The parameter is correct. The race is the unsynchronized append to `out`, and the concurrent use of `programCache`.

## Findings

### 1. Authorization is a mutation argument

Severity: critical.

`resolveCreateProgram` and `resolvePublishProgram` allow the write when `args["role"] == "editor"`. The client supplies `role`. There is no authenticated principal on the request context.

Impact: anyone who can reach `/graphql` can publish or create programs.

Fix:

- Authenticate at the edge (gateway or middleware) and put a verified principal on the request context.
- In the resolver, read that principal. Do not accept role, user id, or tenant from GraphQL arguments.
- Authorize against the program's network (or equivalent ownership), not against a global string.
- Return a GraphQL error with a stable code. Do not include the policy details.

### 2. `filter` is a caller-built MongoDB query

Severity: critical.

`FindPrograms` runs `bson.UnmarshalExtJSON` on the `filter` string and uses that document as the `Find` filter. A caller can send operators (`$ne`, `$where`, `$expr`, `$or`). `network`, `published`, and `title` are overwritten only when those arguments are also set; every other key is queried as sent. A filter that fails to parse is logged and then ignored, so the find still runs.

Fix:

- Delete the `filter` string argument.
- Add an input type with the fields the product supports (`network`, `published`, maybe `updatedAfter`).
- Build the `bson.D` in the store from those typed values.
- Ignore unknown keys. Never merge a decoded client document into a query.

### 3. Search is an unescaped regular expression

Severity: high.

`search` is passed to `$regex` with case-insensitive matching. Mongo evaluates it as a pattern, not as a literal substring.

Impact: a crafted pattern can pin a Mongo thread. Operators inside the pattern change which titles match.

Fix:

- Escape the term with `regexp.QuoteMeta` before `$regex`, or use a text index / prefix match.
- Reject terms over a small max length (for example 64) and reject empty searches that would become `/.*/`.
- Put a `maxTimeMS` on the find.

### 4. Sort field is caller-controlled

Severity: medium.

`sort` is used as a BSON key with no allowlist.

Impact: clients can sort on unindexed or internal fields and force an in-memory sort of a large scan.

Fix: allow `title` and `updated_at` only. Anything else is a validation error. Pair the allowed sorts with a compound index (finding 12).

### 5. Published programs do not round-trip

Severity: high. Correctness.

Three names for one fact:

- `CreateProgram` and `PublishProgram` write `is_published`.
- `Program.Published` decodes `published` (`bson:"published"`).
- `FindPrograms` filters on `published`.

`Description` has the same split: the insert writes `description`, the struct decodes `desc`.

Impact: `publishProgram` reports success, then `program.published` stays false and `programs(published: true)` is empty. Descriptions always come back empty.

Fix:

- Define one Go struct and use it for every insert and update. Do not insert the GraphQL args map.
- Use one BSON name, `published`, and one name, `description`.
- Add a test: create, publish, fetch by id, list with `published: true`, and assert title, description, and published.

`PublishProgram` also ignores `MatchedCount`. A missing id still returns `true`. Treat `MatchedCount == 0` as not found.

### 6. Pagination skips the first page and does not bound the read

Severity: high.

```go
skip := int64(page * limit)
findOpts := options.Find().SetSkip(skip).SetLimit(int64(limit))
```

`page` defaults to 1. Page 1 with limit 20 skips 20 documents. The driver treats limit `0` as no limit, and the argument is optional, so an omitted limit reads the whole collection. Negative limits are not rejected. `cur.Err()` is ignored after the loop.

Impact: wrong pages, and a single query can exhaust memory on the service and on Mongo.

Fix:

- Default `limit` to 20. Allow 1 through 100.
- `skip := (page - 1) * limit`, and reject `page < 1`.
- Prefer keyset pagination (`updated_at` + `_id`) once the list is large. Offset skip gets slower as the page grows, and it shifts when rows are inserted.
- Check `cur.Err()` and return it.

### 7. Request cancellation is discarded, twice

Severity: high.

`graphQLHandler` calls `graphql.Do` with `context.Background()`. Every store method also uses `context.Background()`, so fixing only one layer is not enough. The comment above `graphql.Do` says a slow query should finish after the client leaves.

Impact: a client disconnect or a deadline does not stop the Mongo work. Canceled traffic still occupies the pool. `defer cur.Close(context.Background())` has the same shape.

Fix:

- Pass `r.Context()` into `graphql.Params.Context`.
- Pass `p.Context` into every driver call, including `Close` and `Ping` is unrelated.
- Set a server-side timeout on that context (a few seconds) so a client that stays connected cannot hold a cursor open.
- Set `ReadHeaderTimeout`, `ReadTimeout`, and `WriteTimeout` on `http.Server`. `ListenAndServe` uses none, so slow clients can hold connections.

### 8. Airings resolver races and fans out one query per id

Severity: high.

`resolveAirings` starts a goroutine per airing id and appends to `out` with no synchronization. `GetAiring` is a `FindOne` per id. `GetAiring` turns every error, including a down database, into `(nil, nil)`, so those ids disappear and the parent query still succeeds.

Impact: `go test -race` fails. Under load the process can crash on a concurrent map-style slice growth, drop airings, or open one Mongo operation per id with no cap.

Fix:

- One `Find` with `_id: {$in: ids}`, using the request context.
- Keep order with a map from id to airing if the product cares about order.
- If a later requirement is per-request batching across programs, a DataLoader belongs at the request scope, not a process-global cache.
- Return decode and driver errors. Map `mongo.ErrNoDocuments` to "missing" only at the single-get edge, and do not do that inside a batch.

The `go func(id string)` parameter is the right way to pass the loop value. Do not "fix" that.

### 9. Process-global cache is stale, racy, and wrong for more than one instance

Severity: high.

`programCache` is a bare map. `GetProgram` and `FindPrograms` read and write it from request goroutines. `PublishProgram` never deletes the entry. There is no TTL. The README says more than one instance will run; the map is not shared across instances anyway.

Impact: concurrent requests can crash the process (`concurrent map read and map write`). After the first read, that instance serves the pre-publish document forever.

Fix: remove the cache from this PR. The access pattern is a primary-key read and a filtered list; Mongo with the index in finding 12 is the right first design. If a cache comes back later, it needs a TTL, invalidation on write, a mutex or `singleflight`, and a stated cross-instance story.

### 10. Slug uniqueness is check-then-insert

Severity: medium.

`CreateProgram` uses `CountDocuments` on `slug`, then `InsertOne`. Two requests can both observe zero. `_id` is `UnixNano` as a string, so the unique `_id` index does not protect `slug`. Those ids are also guessable.

Impact: duplicate slugs. A client that can guess ids can probe `program(id:)`.

Fix:

- Unique index on `slug`.
- Insert first. On duplicate key (`11000`), return a conflict.
- Generate ids with `primitive.NewObjectID()` (or a UUID) on the server. Do not take `_id` from the client.

### 11. Publish is three writes, and two of them cannot fail the mutation

Severity: medium.

Publish updates the program, inserts an audit, and updates airings. Audit and airing errors are discarded. There is no transaction. The program document and its airings can disagree, and a publish can exist with no audit row.

Fix:

- Do the program update and the airing update in one MongoDB transaction if they must be atomic. Check `MatchedCount` on the program update inside that transaction.
- Make the audit append-only and retryable, or write it in the same transaction if the product requires it.
- Return the updated program, not a bare boolean, so the client sees the stored document.
- Log the real error server-side. Return a generic failure to the client (finding 13).

### 12. No index for the list query

Severity: medium. Operational, even though the PR has no migration file.

The list filters by `network` and `published` and sorts by a caller-supplied field. Nothing in the PR creates an index.

Fix: compound index `{ network: 1, published: 1, updated_at: -1 }` once sort is allowlisted and `updated_at` is a `time.Time`. Confirm with `explain` that the query is an index scan bounded by the limit.

### 13. Failures are either hidden or returned raw

Severity: medium.

- `GetProgram` and `GetAiring` log the driver error and return `nil, nil`.
- `FindPrograms` returns an empty list when `Find` fails, skips documents that fail to decode, and ignores `cur.Err()`.
- `CreateProgram` and `PublishProgram` wrap the driver error with `%w` and the resolver returns it. `graphql-go` puts `Error()` in the response.
- `NewStore` logs the full URI, password included, when connect or ping fails, then still serves traffic.
- `/health` always writes 200 and never pings Mongo.

`resolveProgram` logs `program.Title` before it looks at the error. `GetProgram` returns a nil pointer for both "missing" and "Mongo is down", so this log panics and the request never becomes a GraphQL error.

Impact: clients cannot tell empty from broken, except when a write happens to leak a server error. Operators get a health check that stays green. Credentials land in logs.

Fix:

- Store methods return wrapped errors. Translate `ErrNoDocuments` to a not-found result only in `GetProgram`.
- Log the wrapped error with a redacted address (host, db, no userinfo).
- Resolver maps store errors to a safe message plus a stable extension code. Keep the driver text in logs.
- Nil-check before logging fields. Missing program returns a not-found error; the schema says `Program!`.
- Health returns 503 when ping fails. Startup should fail the process if the initial ping fails, or the readiness probe must not go green.

### 14. Search fires an unbounded background HTTP call

Severity: medium.

Each non-empty `search` starts a goroutine that calls `http.Get` with the raw search string concatenated into the URL. There is no timeout, no cancellation, no `QueryEscape`, and no cap on in-flight calls.

Impact: slow or stuck analytics calls leak goroutines for the life of the process. Characters such as `&` add extra query parameters on that internal URL.

Fix: remove this side effect from the read path. If the product still wants it, put a bounded worker in front of a client with a timeout, and pass the term as an escaped query parameter. A search must not depend on it.

### 15. Writes accept the GraphQL args map, and inputs are otherwise unchecked

Severity: medium.

`CreateProgram` inserts `args`. That persists `role`, stores `description` under the wrong name (finding 5), and will persist whatever argument is added next. `title` and `slug` are `NonNull` but empty strings pass. `rating` has no range.

Fix: map arguments into a `Program` (or a create-input struct) field by field. Validate slug format, non-empty title, and rating range in that constructor. Reject the rest.

### 16. The HTTP endpoint has no abuse limits

Severity: medium.

`io.ReadAll` on the body has no cap. There is no query depth or complexity limit, so a nested or aliased query can multiply the airings fan-out. GET serves a GraphiQL page in every environment. `Access-Control-Allow-Origin` is `*`. Invalid JSON is ignored and the empty query still runs, with HTTP 200.

Fix:

- `http.MaxBytesReader` (for example 64KiB) before reading.
- Reject non-POST and invalid JSON with 400.
- Disable GraphiQL except in local development.
- Add a depth limit and a complexity limit. Aliases must count.
- Restrict CORS to the known web origins. Do not combine `*` with credentialed browser calls.
- Keep HTTP 200 for GraphQL field errors. That part is fine.

### 17. Timestamps are display strings

Severity: low.

`updated_at` is `time.RFC822`. Those strings do not sort in time order, and the zone abbreviation is not a reliable offset.

Fix: store `time.Time` in UTC. Format it only at the GraphQL edge, as RFC3339.

### 18. Nothing tests the behavior above

Severity: medium, as part of the review, not as a standalone lecture.

The PR has no tests. The bugs in findings 5, 6, 8, 10, and 13 are all deterministic without a full server.

Ask for:

- Store tests, using `mongo/integration/mtest` or a fake collection interface, for publish-then-read, page 1 returning the first page, duplicate slug, unknown id, and a `filter` payload that must be rejected rather than executed.
- A race test around loading airings for one program.
- A resolver test that a principal without access cannot publish, and that a `role` argument cannot grant access.

## Suggested order if you discuss the review live

1. Who is allowed to publish, and how the code decides.
2. What a client can pass as `filter` and `search`.
3. Why publish looks successful and the following read does not show it.
4. What happens to an in-flight `programs` query when the client hangs up.
5. What `go test -race` does with `resolveAirings` and `programCache`.

Stop there if time is short. Findings 10 through 18 are the second pass.

## Follow-up questions

Use a few, based on what they did not already say. A strong answer is specific to this handler. A weak answer restates the question.

1. **Where should authorization live if a REST handler is added next to this GraphQL handler?**
   Strong: one middleware authenticates and stores the principal on the context; each use case checks authorization on the loaded program. The GraphQL layer does not grow its own role argument. They mention the check has to use the persisted network (or owner), loaded from Mongo, not a field the client repeats in the mutation.

2. **Walk through a client disconnect during `programs`.**
   Strong: today nothing stops, because the handler replaced the request context and the store never reads `p.Context`. After the fix, cancel propagates to `Find` / `Next` / `Close`, the driver returns `context.Canceled`, and the resolver does not fill the cache with a partial decode. They also put a deadline on the server side so a client that does not disconnect still cannot run forever.

3. **Offset pagination versus a continuation token for this list.**
   Strong: offset matches the current `page` argument but page 1 is already wrong, and `skip` costs more as the offset grows. A token of `(updated_at, _id)` is stable when new programs are inserted. They tie this to "store `time.Time`" and to the compound index. They do not propose a cursor scheme without a sort tie-breaker.

4. **How do you stop one GraphQL query from taking Mongo down?**
   Strong: they stack limits instead of picking one. Typed filters, a max limit, `maxTimeMS`, query depth, complexity that counts aliases, and no per-airing fan-out. They mention the search regex as a separate DoS from nesting. They refuse a fix that is only "add a rate limit at the gateway" and leaves the unbounded `Find` in place.

5. **Would you use a multi-document transaction for publish?**
   Strong: yes if program and airings must flip together, with the caveat that the collections have to be on a replica set and the transaction should be short. They still check `MatchedCount`. If audit volume is high, they are willing to put audit on an outbox rather than inside the transaction. "Always use a transaction" and "transactions are slow so ignore the partial write" are both incomplete.

6. **The cache author says it makes hot programs cheap. What do you ship instead?**
   Strong: delete it in this PR. Primary-key reads are already cheap with `_id`, and a process-local map is actively wrong once two instances and publish exist. If they keep a cache, they specify TTL, explicit delete on publish, synchronization, and why a single-process cache is acceptable. Bonus: request-scoped DataLoader for airings is a different tool and does not belong in a global map.

7. **What tests do you want before merge, and what is not worth a test?**
   Strong: they list the round-trip, the first page, the duplicate slug under concurrency, and an unauthorized publish. They do not ask for a test that GraphiQL HTML contains a particular sentence, or for a snapshot of the whole schema as the only coverage.

8. **How should a Mongo outage look to the client and to the health check?**
   Strong: reads and writes return a generic error, logs hold the driver error without the URI userinfo, and readiness goes 503. They notice today's code does the opposite on reads (`nil, nil` and an empty list) and panics in `resolveProgram`. Liveness can stay up if the process is fine; readiness must not.

9. **`createProgram` inserts the args map. What breaks when we add `ownerId` next sprint?**
   Strong: that field is persisted automatically, including whatever the client sent, and it skips validation. They want a struct literal assigned field by field, so a new argument is not stored until someone maps it. This is the mass-assignment version of the same bug as `role`.

10. **What would you leave alone in this PR?**
    Strong: HTTP 200 for resolver errors, the `id` parameter on the airing goroutine, and JSON struct tags as the GraphQL default-resolver contract. They can mention `gqlgen` as a later choice, not as the fix for injection or auth. A review that starts with "rewrite it in gqlgen" is avoiding the defects.
