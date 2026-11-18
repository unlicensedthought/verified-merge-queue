# verified-merge-queue

A small merge queue that moves trunk only to a commit it has verified.

## The problem

A merge queue that rebases a change, runs the tests, and then merges "the change" has a gap.
What got tested and what lands can differ: trunk moved, the rebase was redone, the tests ran on a
different tree. The only safe rule is that the commit trunk advances to is the commit the checks ran on.

`vmq` enforces that. For each land request it builds one candidate commit on top of the current trunk
tip, checks out that exact commit, runs your verify command, and pushes **that commit** as a
fast-forward. If trunk moved while verifying, the pass is thrown away and the request is rebuilt and
verified again on the new trunk.

## Quick start

```sh
go build -o vmq .
cp config.example.json vmq.json      # set repos[].remote and verify_cmd
./vmq --config vmq.json --shadow     # verify and record, change nothing
./vmq --config vmq.json              # land for real
```

Ask to land by pushing a commit to a request ref on the remote. `<n>` is a positive integer, unique per request:

```sh
git push origin HEAD:refs/land/<n>
git push origin HEAD:refs/land-first/<n>   # optional, also: sorts ahead of the queue
```

Use one squashed commit per request. Its subject and body become the trunk commit message.

`verify_cmd` is run in the candidate checkout as `<cmd...> --base <trunk-sha>`. It must:

- exit 0 for pass, 1 for fail, anything else for "could not run";
- print `VERIFY {json}` as the last line on stdout.

Exit 0 without that line is treated as could-not-run, not as a pass.

## How it works

1. Poll `refs/land-first/*` and `refs/land/*`. First-label requests sort ahead; within a group the order is the request head's committer date, oldest first, then request number. The queue is polled again before each request.
2. Fetch trunk and the request head. Rebase the head onto trunk. A conflict rejects the request.
3. Squash the rebased tree into one commit on the trunk tip: `<subject> (#<n>)`, the body, and a `Co-authored-by` trailer for each distinct author. A request that changes nothing is rejected.
4. Check out that commit and run `verify_cmd`. Pass continues; exit 1 rejects the request; anything else backs off (60 s, then 120 s) and gives up after 3 tries.
5. Check the remote trunk is still the tip the candidate was built on, push `<candidate>:refs/heads/<trunk>` with no force, then read trunk back from the remote. The read-back is the proof; git's exit code is not.
6. If trunk moved while verifying or before the push, go back to step 2, up to 5 times, then treat it as could-not-run.
7. After a land: run the optional `after_land` commands, write a note to `<workdir>/results/<name>/<n>.md`, delete the request refs.

Every outcome is appended to `<workdir>/verdicts.jsonl`, with the head, trunk tip, candidate, tree and the verify payload.

**Shadow mode** (`--shadow`) runs steps 1 to 4, records the verdict in the JSONL log, and stops. It
never pushes, never writes results back, never deletes request refs and never runs `after_land`.
Run it beside your current process until its verdicts match what you would have done.

The git host is behind one interface (`Intake` in `intake.go`). The included implementation uses
plain refs on the remote, so it works with any git server that lets you push refs. To drive it from
pull-request labels, implement `Intake` for your host.

## Limits

- It serves a trunk branch with one writer: the queue. Anything else pushing straight to trunk just causes re-verifies.
- One request at a time per repo. Throughput is one verify per land.
- No batching or speculative merging of several requests.
- It trusts the verify command. A verify that exits 0 and prints a valid `VERIFY` line is a pass.
- Only squash landing. No merge commits, no preserving request history.
- No authentication layer: git credentials come from the environment the daemon runs in.
- No host-load admission, resource accounting or signing of the audit log. The JSONL log is append-only by convention, not by protection.
- A stop signal lets the running land finish and starts nothing new; a second signal exits at once.

## Tests

```sh
scripts/verify.sh        # deps, build, vet, gofmt, tests; last line is the verdict
go test -count=1 ./...   # tests only
```

The tests run against a throwaway bare repo. The test binary doubles as the fake verifier, so no
network, no scripts and no real credentials are involved. Among them: the landed commit is the one the
verifier ran on; a failing verify leaves trunk unchanged; trunk moving mid-verify forces a second
verify; a non-fast-forward or stale-tip push is refused; shadow mode leaves trunk, refs and results untouched.

## License

Apache-2.0. Copyright 2026 Adam Scheurer.
