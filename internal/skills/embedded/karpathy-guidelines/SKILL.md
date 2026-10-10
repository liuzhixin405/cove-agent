---
name: karpathy-guidelines
description: "Karpathy-inspired guidelines: Think before coding, Simplicity first, Surgical changes, Goal-driven execution, Evidence before code, Hand-off notes"
---

# Karpathy-Inspired Coding Guidelines

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

**Core principle:** every premise you write code on must point at evidence — a code line, a command's output, the user's words, a git record. A premise you only suspect is something to verify or ask about first, not to build on.

## 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State the target behavior in one sentence and name where the evidence for it comes from.
- List the consumers (who calls this, how many at a time, which fields they read) and the external dependencies (batch support, concurrency limits, what happens to a write after a timeout). Mark each item **confirmed / to ask / my assumption**.
- When "to ask" items remain, what you hand over is the list of questions, not an implementation. Build only the parts that hold whatever the answers turn out to be.
- If multiple interpretations exist, present them — don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- For performance work, point at the slow segment first, then choose a fix.

In an existing codebase, read the site first:
- Follow the project's conventions: its instructions file, contributing guide, lint config, and how the neighboring module does the same thing. Don't introduce a new pattern next to an existing one.
- Check history before adding: `git log --oneline -i --grep=<keyword>` and `git log -S<symbol>`. A hit on revert, retire or drop usually carries the answer in its commit body. Something the docs call "implemented" that is missing from the code was probably withdrawn — find out why before putting it back.
- Check for prior decisions ("decided not to do") in memory or discussion notes; cite them instead of re-investigating.
- Before "doing it like the other module", confirm the precondition that pattern relies on (a real batch endpoint, for instance) also holds here.

## 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.
- Get the thinnest complete path working first; stub external dependencies until it does.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

## 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code or a neighboring bug, mention it — don't fix it. Pre-existing violations are not yours; new violations on lines you changed are.
- Once you have the root cause, grep every read and write of the same field or condition and list them. Fixing one path of several rarely fixes the bug.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

## 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

Then try to falsify the result:
- Revert the implementation and run the new test: it must go red. A test that stays green never tested the fix.
- An unconfigured mock returns empty collections or null; an assertion on top of that is an assertion on empty data.
- Pin assertions to concrete old and new values of specific fields. `not null` and `Count > 0` prove nothing.
- When an existing test's expectation has to change, write down how the new value follows from the inputs. Backfilling it from the new output launders a bug into green.

## 5. Second-Round Feedback

The second time the same work comes back as "still wrong": stop. Write the target **behavior** in one or two sentences, get it confirmed, then change the code. Don't stare at the screenshot and guess another version.

## 6. Hand-Off Notes

When reporting done, say:
- Where the change takes effect: a code default or a config file, and whether deployment overrides or merges that config.
- Deployment order and any version that has to be bumped.
- Which checks you ran, with a real excerpt of their output.
- Problems you found and did not fix, listed separately.

## Red Flags

Stop when you catch yourself thinking:
- "The caller probably…", "the external API should support…", "the user most likely wants…"
- "Everywhere else does it this way, just copy it"
- "Change the expected value and the test passes", "the new test passes" (without having reverted the implementation)
- "I'll fix that neighboring thing while I'm here"
- "Get it running first; tests, CI and logs later"
- "No time to ask, go with the most likely reading"

| Excuse | Reality |
|---|---|
| "Asking the user is slow" | One batch revert costs an order of magnitude more than one question |
| "Copying the existing pattern is safest" | Copying a pattern whose precondition you never checked is copying an assumption |
| "The mock is configured, the test is meaningful" | Only a test that goes red when the implementation is reverted proves it tests the fix |
| "No real data yet, the format can change freely" | The format freezes the moment the first real user writes to it |

---

**These guidelines are working if:** fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, fewer reverts traced to an unverified premise, and clarifying questions come before implementation rather than after mistakes.
