# Where testing effort pays

Five rules, each of which was learned the expensive way in this repository. The
examples are real and the commit history has them; they are here because the
general form is not obvious from the specific bug.

## 1. Aim at the boundary where untrusted input enters

**Before writing the happy path, list every value that arrives from outside the
process and ask what happens when each one is hostile.**

The MegaRAID work is the case study. The pure decoders — SCSI log pages,
INQUIRY, the controller configuration — were tested thoroughly from the start.
The *transport* was treated as plumbing. But the transport is where the numbers
from firmware arrive, and four review rounds each found something there:

| value from outside | what it did |
|---|---|
| `args[3]`, the HDIO sector count | we sent `0x4F`; the kernel wrote 40,448 bytes into a 516-byte stack buffer (#20) |
| the controller's declared config length | a reply declaring `1` truncated the buffer and the next read panicked |
| the SCSI status byte | `CHECK CONDITION` — *"that command failed"* — was recorded as a successful reading |
| a log page's parameter lengths | followed without bounds checks would walk off the buffer |

Those are four instances of one mistake, and enumerating the inputs first would
have found them **together, in one pass, before any review**. Testing the
decoders harder would not have found any of them.

The general form: **a length or a status that comes from outside is an
assertion, not a fact.** It is a number a device gave us, and it must be checked
against what is possible before it is used to size memory, slice a buffer, or
decide that something succeeded.

## 2. Assert what the user sees, not what the component computes

The fleet dashboard once rendered:

```
SMART unhealthy
0 of 6
```

on every healthy host. Five tests asserted on that value — `0 of 6`, `1 of 3`,
`2 of 4` — and **every one was correct and passing** while the card told a human
the opposite of the truth. Nobody had asserted on what it sat under.

The test that catches it does not look at the value at all:

```ts
await expect(healthyCard).not.toContainText('unhealthy');
```

It reads the whole card as text, which is what the owner did when he reported
it. Pair it with the inverse — *an unhealthy host still says so* — or "delete
the word" passes.

A test of the data path wearing a test of the interface as a costume is the
most comfortable kind to write and the least likely to catch what matters.

## 3. A fixture can fail in six different ways

All six occurred here, and only one of them announces itself.

**Too regular.** A property test generated field names `f0`, `f1`, `f2`, so no
array ever sat at a keyed path and the keyed-diff branch was never exercised.

**Symmetric, so both hypotheses give the same answer.** A RAID fixture had one
online-but-silent drive and one offline-but-answering drive, so counting
*online* and counting *SMART-readable* both returned three. The test held
against a right implementation and a wrong one alike.

**Asserting the opposite of what it claims.** `{Level: "raid1"}` with no
`State` looked like a healthy array and was, by the code's own deliberate rule,
*non-operational* — because an array whose state cannot be read is reported
rather than assumed well. The "healthy" fixtures were not healthy.

**Encoding the behaviour under test.** A Playwright fixture said
`sync_action: 'idle'` because that is what the collector produced — and it
failed the moment the collector stopped producing something wrong. **This is the
benign member of the family: the others hide, this one confesses.**

**Unreachable by construction.** A member temperature override was set to
100 °C on a 70 °C drive, so only the *warning* branch could ever decide — and a
critical branch still reading the wrong threshold was untestable. A fixture
where one of two paths cannot be reached tests half the change.

**Testing the harness's configuration instead of yours.** `pkg/server` reaches
its handlers through `httptest.NewServer(s)`, which builds an `http.Server` of
its own — so it carries none of the timeouts the production constructor sets. A
response deadline asserted through it would have been asserting *httptest's*
configuration, and passing. Nothing in the package had ever tested the server
configuration at all, which is why a missing `WriteTimeout` survived every test
in the suite for the life of the package. The tests for it run the configured
`http.Server` on a real listener instead. **Ask what the harness supplies before
believing the assertion is about your code.**

The same failure occurs outside tests, in any query you trust. An audit for
misconfigured git remotes globbed every repository on a workstation and
returned 25 confident results, most of them third-party checkouts that are not
company repositories at all: **a precise answer about the wrong population.**

**The trigger is an answer that disagrees with something you already know**, not
suspicion of the method — a check run carefully feels the same as one run
carelessly. A diff claiming five branches were unmerged was caught because
somebody had watched those five merge; it was measuring how far `main` had
moved, not whether anything was merged.

**And the trigger only fires where you have independent knowledge to be
surprised by.** The follow-up check — which branches still exist on the server
— was wrong too and went unnoticed, because nobody knew the answer in advance.
It reported four branches that had already been deleted.

So: **when the question is about a remote system, ask the remote system.**
`git branch -r` answers from a local cache; the API answers from the source.
And the cache does not refresh the way its name implies:

```console
$ git fetch dev          # 7 branches listed, 4 of them long deleted
$ git fetch dev --prune  # 2 branches, which is the truth
```

`git fetch` adds what is new; it does not remove what is gone. Refreshing
first would not have helped, which makes this rule 5 wearing a different hat —
**a name resolving to something other than what the reader assumed**, in the
tool being used to check.

### Named traps

General caution about instruments has not saved anybody here. Each of these has
caught somebody at least once, most of them more than once, so they are written
down as specific things to recognise rather than as advice to be careful. Add to
the list rather than restating the general principle.

**`pgrep -f <pattern>` matches the shell running it.** The check's own command
line contains the pattern, so it finds itself and reports the process alive.
It has produced a false "still running" for a process that had exited, and a
false "capture in progress" for a capture that never started.

```console
$ pgrep -f "myprocess serve"      # matches the bash -c running this line
$ pgrep -f "myprocess serve" | grep -v $$   # or match the executable, not the args
```

**Asking a JSON payload for a field name that does not exist returns nothing,
and nothing reads as absence.** A reader asked members for `bay`, `position`
and `temperature_c`; the payload calls them `enclosure`, `slot` and
`temperature_celsius`. Every drive came back with `None` for each, and the
report was "the data is missing" rather than "my key is wrong" — **`absent`
versus `zero`, with a JSON key as the name.** A decoder into a typed structure
fails on an unknown field; a dictionary lookup does not.

**A "wait" that burns a core.** `read -t 15 x < /dev/zero`, used as a sleep,
waits the right amount of wall time and consumes a **whole CPU** doing it —
measured: 3.00 s elapsed at 99% of a core (1.22 user + 1.77 sys), against 0%
for `sleep 3`. It is not wrong about the duration, which is why it survives
review. It is wrong about the cost, so any measurement it is waiting *for* —
CPU time, throttling, load, a quota — is being perturbed by the wait itself.

**Grepping a dump for a format that changed.** A search for `goroutine N [` in
a Go stack dump returned zero, because the format is
`goroutine N gp=0x.. m=nil [state, N minutes]`. Zero read as "no goroutines in
that state" rather than "my pattern is wrong" — `absent` versus `zero` again,
this time with a text format as the name. It was caught only because a second
grep beside it, for the *frames*, returned 118: **two probes disagreeing is what
exposes this, not either probe alone.**

**A field that reports the same value for two opposite states.** `git log
--format=%G?` prints `N` for an unsigned commit *and* for a correctly signed one
that cannot be verified without `gpg.ssh.allowedSignersFile`, which agents do
not have. Measured on this repository's own `main`:

```console
$ git log -1 --format=%G? 5edd5b65      # N
$ git cat-file commit 5edd5b65 | grep ^gpgsig   # present
```

A check built on `%G?` reports `N` on correctly signed commits, would fail
everything, and would be switched off within a week. **"Is there a signature"
and "does the signature verify" are different questions**, and only the first
can be answered without the file.

**Recomputing a combined view from the raw list.** A commit's status was read by
paging the per-context status list and keeping the first entry per context.
Four entries existed for one aggregator and two shared a timestamp to the
second, so first-in-list picked `pending` over `success` and a green PR was
reported red — then reported to a third party as "merged on a pending check",
which had not happened. The API has a combined-status endpoint that computes
exactly this. **When an API offers a combined or computed view, do not
reconstruct it yourself from the raw list**; an ambiguous ordering is not a
verdict.

**A guarantee that holds for a reason nobody wrote down.** Signed commits are
required on `main`, and unsigned commits were found on a feature branch — which
looked like the guarantee failing. It was not. Measured: a Gitea **squash merge
discards the branch's commit objects entirely** and authors a new single-parent
commit signed with its own key, so a branch's signatures never had a path onto
`main`; and `main`'s protection separately sets `require_signed_commits`, which
covers the direct-push path.

Two mechanisms, both undocumented, and the investigation began by assuming
neither existed. **Before reporting that a guarantee is broken, find out what
was actually enforcing it** — the answer changes the severity, and here it
changed it downward.

The strongest remedy is to stop writing the fixture. `pkg/collector/testdata/`
holds 1120 bytes captured from a real controller, and it caught a decoder bug
within the hour: that machine's RAID10 lays two spans over *one* array, so
walking spans returned every drive twice. A fixture written from "RAID10 means
two arrays" would have encoded the same wrong belief and passed.

## 4. Mutate the change, including the one you are sure about

Break the behaviour, confirm a test fails, restore it. A test that passes
against the broken version is worse than none.

Do it even when the change is obviously correct, because that is where it earns
the most. A review reported that an `EPERM` from `open(2)` was being
mishandled; the fix was applied before checking, and **the mutation proving the
fix was necessary survived** — which is how it emerged that
`errors.Is(&os.PathError{Err: syscall.EPERM}, os.ErrPermission)` is already
true, and the "fix" changed nothing. Without that step, a redundant clause and a
comment asserting an impossible hazard would both have shipped.

Two related habits:

- **A mutation that survives is usually a gap in the fixture, not in the code.**
  Every survivor above was a missing case, not a missing assertion.
- **A green suite does not mean a hardware path was exercised.** The MegaRAID
  ioctl cannot run in CI. What it gets instead is a live run on the one machine
  that has the controller, and a sentence in the docs saying that is what it
  got. After tightening the SCSI status check, the live run was the only thing
  that could show it still *accepts* what it must — tests only showed it rejects
  what it should.

## 5. A name is not a thing

Three defects in one small CI change, in three unrelated layers, none of them a
logic error:

- **The Makefile checked one path and ran another.** `make lint` tested `PATH`
  for `golangci-lint`, found the binary the runner image bakes into
  `/usr/local/bin`, passed — then invoked `$(GOBIN_DIR)/golangci-lint`, which
  does not exist there. A check and its subject disagreeing about which file
  they mean.
- **A version pin guarded against a dependency this repository never had.** It
  avoided a staticcheck RC that panicked on `sentry-go`; `go.sum` has never
  contained `sentry-go`, and the released version had fixed it besides.
- **A runner label named two different machines.** `runs-on: ubuntu-latest` is
  our image on Gitea and GitHub's hosted image on github.com. A fix that
  required a file only our image has was correct in one place and inoperative
  in the other.

**All three are a name resolving to something other than what the code
assumed.** None looks wrong on inspection, because each is a correct check
applied to the wrong referent.

The same shape appears wherever two things share a name and are not the same
thing: a fleet cell counting one set of devices while its label described
another; a SMART badge whose heading described something different from the
value beneath it; an alert filed under `sdb/64:7` whose thresholds were read
under `sdb`, so an override written for one drive was unreachable and one
written for the volume silently governed every member.

The check that catches these is not a stronger assertion. It is asking, of each
name in a change: **what does this resolve to, here, at the moment it is used —
and is that the same thing the code beside it means by it?**
