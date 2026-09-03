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

## 3. A fixture can fail in four different ways

All four occurred here, and only the last announces itself.

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
