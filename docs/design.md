# quarry — bounded recursive decomposition with verified provenance

**Status:** design, pre-implementation. Nothing here is built.
**Maturity (agate matrix):** Seam.
**Name:** `quarry` is provisional — you quarry bedrock, and quarrying is extraction by
controlled breaking. Replace it freely; it appears only in the package name and the CLI verb.

---

## 0. What this is

A system that takes a research-shaped problem, rewrites it into sub-problems dispatched to
independent agents, recurses to a bounded limit, and returns an answer together with a record
of how the answer was produced and how much to trust it.

Three things it is not:

- **Not an agent framework.** [agenkit](https://github.com/scttfrdmn/agenkit) is that. quarry is
  a scheduling and accounting discipline expressed *in* agenkit's interfaces.
- **Not a governance layer.** [agate](https://github.com/scttfrdmn/agate) is that. quarry consumes
  agate's identity scoping, cap enforcement, and metering rather than reimplementing them.
- **Not a chatbot.** The deliverable is a run record with a cost receipt and a stability
  estimate. The prose answer is one field in it.

Division of labour, following agate's existing formulation — *agenkit builds the agent, agate
governs it* — quarry decides **how many agents, arranged how, for how much**.

### Build posture

Phase 1 is a standalone package with no AWS dependency, following the `cost/` convention in
agate: pure logic, testable without credentials, driven by an in-process executor and a fake
provider. Phase 2 wires it behind the agate chokepoint and gives it a stack. The standalone
version is not throwaway — it remains the test harness.

Assumed stack unless overridden: Python 3.12/3.13 for the core (matches agate), Go if the
executor later needs to be a separate service.

---

## 1. Governing principles

Each principle states a rule, the reason it exists, and what it forbids. If an implementation
decision contradicts one of these, either the decision or the principle is wrong — say which.

### P1. Decompose only where surface-to-volume favours it

A sub-problem needs context replicated into its prompt (*surface*) in order to do reasoning
alone (*volume*). When surface approaches volume you pay near-full cost per child and buy only
latency.

*Consequence:* decomposition is a property of the problem, not a default strategy. Code
qualifies often — interfaces are narrow, type signatures are the halo. Open-ended synthesis
frequently does not. The planner must be able to decline to split.

### P2. Recurse only as deep as you have verifiers

Error compounds along depth. With a cheap local check, failures are caught and retried in
place and depth is nearly free. Without one, expected quality decays geometrically and the
merge becomes an act of faith.

*Consequence:* verifier availability, not `max_depth`, is the primary termination condition.
`max_depth` remains as a backstop, not as the design.

### P3. Verification spend is proportional to downstream exposure

Verification value scales with the cost of what it gates. This is Young/Daly with the terms
swapped: optimal checkpoint interval balances checkpoint cost against expected lost work;
optimal verification density balances verify cost against expected rework from an error caught
late. Error rate replaces MTBF; subtree cost replaces rework.

*Consequence:* never verify uniformly. The planner's decomposition is the cheapest node in the
tree and gates every dollar below it — verify that hardest, adversarially, before any fanout.

### P4. The cap is the contract; the estimate is a courtesy

A cap is admission control — enforceable at every node, roughly a hundred lines. An estimate is
a prediction of tree size, which is a research problem.

*Consequence:* caps ship first and are mandatory. Estimates are a readout derived from the
calibration corpus that running under caps produces. Never gate a feature on estimate quality.

### P5. NO CLOCKS constrains the floor, not the ceiling

Idle cost must approach zero. Spending hard during active work is exactly what the budget is
for.

*Consequence:* a live orchestrator process for the duration of a run is fine. Persistent
provisioned capacity is not — no warm worker pool, no Redis or OpenSearch for the cache. Durable
state and memoization are productive storage, bill per-byte, and are explicitly permitted.

### P6. Scope never widens on descent

A child's authority is its parent's authority or narrower, always. A decomposer that mints
sub-agents with broader scope is a confused deputy, and defeats the boundary agate exists to
hold.

*Consequence:* the `agate:` session tags travel with the ledger to every node. Cache keys
include scope (§6). Depth and lineage are audit fields, not debug fields.

### P7. A run is an estimate, not an answer

The model is an instrument with irreducible variance; n=1 is one sample. The planner is a
larger variance source than token sampling, because different decompositions produce genuinely
different answers.

*Consequence:* replicates are default, not an add-on. The budget dial is partly a sample-size
dial. Report unstable claims explicitly rather than averaging them into confident prose.

### P8. The record outlives the model

Models get retired; papers do not. A re-run recipe has a shelf life measured in months.

*Consequence:* the archival artifact is the recorded transcript, replayable indefinitely without
model access. Re-derivation is marked best-effort. Model IDs are pinned with explicit versions,
never aliases.

### P9. Planning is budget-conditioned

Caps are **inputs to planning**, not only boundaries applied to its output. A planner that splits
first and discovers the money later produces a tree that gets truncated wherever it happened to
run out — arbitrary degradation, discovered at minute fifteen.

Caps are plural and come in more than one denomination (§3.1). A run may be capped by spend, by
latency, by due date, or by several at once, and the planner must satisfy all of them
simultaneously.

This principle carries more weight precisely because §4's estimator is weak (§13). A
constraint-satisfying planner does not need a good estimator: it is not predicting cost, it is
fitting a plan to a stated balance. The estimate degrades to advisory — *"this looks like a $40
problem and you gave me $10; here is the reduced scope"* — rather than load-bearing.

*Consequences:*

- The planner's task is "decompose this **given this balance**", at every level of the tree. A
  node that receives an allocation plans within it.
- The planner's output includes a **proposed apportionment**, not just a split (§2, §3).
- Insufficient budget is a legitimate way to exercise P1: if the balance will not support a
  decomposition *plus its verification*, do not decompose.
- Degradation is **planned and disclosed before spend**, not emergent. A tight cap produces
  "I can cover 3 of these 6 sub-questions; here are the 3, here is what is excluded" — reviewable
  at the plan gate (§9).

---

## 2. Model of computation

### Node

A node is an agenkit `Agent` — uniform `process(Message) -> Message`. Recursion is therefore
structural: a decomposing node is an Agent that happens to spawn Agents. No separate orchestrator
type, no leaf/internal distinction in the type system.

```
Node    = Sequential(Planner → Parallel(children) → Reducer)
child   = Node | Solver          # chosen at runtime per P2
```

### Planner output is a plan *and* an apportionment

Per P9 the planner receives the node's balance and must return a split that fits it. It emits
**relative weights** — *this child is roughly 3× the work of that one* — and the system converts
weights to absolute currency (§3). Relative estimation is far more reliable than absolute, for
the same reason story points beat hour estimates, and it sidesteps the calibration gap that makes
§4 weak.

This yields something valuable: a **mechanical verifier for the planner**. Without an LLM you can
check that the apportionment sums within the balance, that no child falls below the floor of one
solve plus one verification, that the reserve is intact, and that declared leaves are not
allocated recursion budget. Per P3 the planner is the node most worth verifying and previously had
only adversarial checking available; budget-conditioning makes part of it checkable for free.

### A plan can be approved before it is executed, and the approval is an artifact

P9's last consequence says degradation is "reviewable at the plan gate (§9)". For a long time
that gate was informational: the TUI *displayed* the split, the apportionment and the exclusions
before fanout, and then fanned out regardless. Reviewing something you cannot refuse is not a gate.

The gate is now two commands. `quarry plan` produces a **plan artifact** — a pinned, content-hashed
file describing one proposed decomposition — and `quarry run --plan <file>` executes that artifact
and nothing else. Between them, a host or a person decides.

**A plan is valid only for the budget it was planned under.** This is the integrity property the
whole gate rests on, and it is P9 read backwards: if caps are inputs to planning, then a plan
approved under one cap is not a plan under another. The same split under half the money is a
different plan, and one the planner might have refused. So the artifact carries the cap it was
planned against, and the second phase **refuses a mismatch** rather than silently re-apportioning.
It refuses on the floor and the depth bound for the same reason, and on a fake-planned artifact
offered to a live run — a synthetic split must not be able to authorise real spend.

**Scope may not widen between the phases** (P6). Descent is not the only place authority can
broaden; a gap between deciding and doing is another, and it is the one where the widening would be
a host's doing rather than a planner's.

**The artifact is identified by a content hash, and the record says which plan it executed.** A run
record that cannot name the plan it was authorised to run leaves the approval unverifiable after the
fact — the receipt would show the money and not the permission. So `PlanID` is a recorded field, and
because it is hashed it is inside the run's own identity: a gated run cannot be re-described as
ungated without changing its `RunID`.

**An edited artifact is refused, not warned about.** This is the one place the record's convention is
deliberately inverted. A record that fails its hash still gets shown with a warning, because it is
history and history is worth reading even when it is suspect. An artifact is not history — it is an
*authorization* — and honouring an edited one would spend money on a split nobody approved while
recording an approval nobody gave. For the same reason, editing a plan before approving it is not
supported: a host-edited plan is one the planner never proposed and may have declined.

**Only the root is approved.** What a host approves is the split of the whole problem and the
division of the money across it; each child then plans within an allocation that *was* approved, so
a child re-planning is not spending authority nobody granted. Approving every node would make the
gate a debugger.

**The second phase restates the problem rather than reading it out of the artifact.** `run --plan`
requires the statement on the command line even though the file already carries it, and the
apparent redundancy is the check: the artifact *confirms* what the caller asked for. A file that
supplied both the question and its own authorisation could not detect a host that pointed at the
wrong plan, which is the likeliest mistake in a directory of them.

Two consequences of that, both of which shipped broken and are recorded because they are the kind
of defect a passing test suite does not see:

- **The command the gate prints must be runnable as printed.** `quarry plan` ends by telling the
  operator how to execute what it just wrote, and every value `Authorizes` compares — cap, depth,
  floor, scope, statement — has to appear in that line, single-quoted, with the statement last so a
  caller's own flags land where Go's `flag` package still reads them. This is one of the few places
  where prose output *is* an interface, and it was wrong: the statement was omitted entirely, so the
  copy-pasteable line exited 2 with usage text. Invisible to the suite because every test built its
  own argv.
- **A refusal must show where two statements differ, not their first sixty characters.** Truncating
  both sides to a common prefix prints two identical-looking lines under the words "a different
  problem" — precisely the case the error exists to explain. The window is centred on the divergence
  and clipped on a rune boundary.

**Declining is a first-class outcome.** A planner that refuses to split (P1) produces a valid,
approvable artifact that runs as a single node — not an error and not an empty file. That case is
the routine one under `--fake`, whose planner declines on clause length, and a gate that treated it
as a failure would make P1 unreachable through the gate.

**The approved fanout must be the executed fanout,** which is less obvious than it sounds. Identical
children collapse before apportionment (the DAG rule, below), so an artifact listing its children
verbatim describes a wider tree than the run will have and divides the money differently. Both the
producer and the authorization check therefore collapse first, using the executor's own function.
Getting this wrong is silent in the worst way: both sides of the comparison are wrong in the same
direction, so the check passes and the run executes a tree nobody approved.

**Planning costs money, and the artifact says how much.** "Near-zero spend" must be a stated number
with its own cap rather than a hope — one planner call is real, and the §4 variance diagnostic is
*k* of them. So the planning phase runs under its own small ceiling, separate from the run's cap.
Debiting it from the run's cap would be the natural-looking choice and it is wrong: it would shrink
the budget the plan was made under, violating the validity property above with the mechanism meant
to satisfy this one.

### How a leaf is told about its budget

P9 is about *planning*, and for a while that is where it was implemented: the planner received
its balance and the leaf — the only thing in the system that actually spends money — received an
`Allocation` and discarded it. That is P9 holding where it is cheap and failing where it counts.
The first live run is the evidence: across 18 answered leaves, generated tokens ran 249 / 381 /
1008 (min / median / max) against a **30-token halo** — a bare sub-question in, an essay out —
while five of thirty nodes went unfunded and 68% of the cap went unspent.

**The budget reaches the leaf as a word count, never as currency.** Same reason weights are
relative (above): a model told *you have $0.002* is being asked to price tokens it cannot see, so
its brevity would track its guess about pricing rather than the budget. A word count is a
constraint the system derived from a number it actually has. Alongside it go shape rules — no
preamble, no restatement, conclusion first, no headings or tables unless genuinely tabular —
which are what make the word count achievable and which also attack §8's structural-claim problem
at its source, since headings and table rows are what the mechanical extractor counts as claims.

**The prompt is a request; the ceiling is the cap.** A model asked for brevity may decline, and
models routinely do, so the prompt alone would be a request dressed as a constraint. A ceiling
alone would truncate mid-sentence with no explanation. Both, and — this is the part that is a
design rule rather than an implementation detail — **derived from one number**, because two
independently configured limits drift and the failure when they drift is silent: a prompt asking
for 400 words under a 200-token ceiling produces confident answers that stop mid-sentence.

The ceiling is clamped below as well as above, and the lower clamp is not a rounding convenience.
A node allocated almost nothing prices out at a handful of tokens, and a handful of tokens buys a
fragment that costs real money, asserts nothing, and enters the record as an *answer*. Deciding a
node is too poor to be worth solving is the **floor's** job (§3), which refuses it outright and
says so; a ceiling that silently degraded the same nodes to fragments would be a second, invisible
floor with none of the first's bookkeeping.

**The recorded replay key is the statement, not the prompt** — so prompt construction belongs in
the Solver, above the Provider. §7's recorded provider indexes samples by the recorded problem and
looks them up by the prompt it is handed; those coincide only while the solver passes the bare
statement. Wrapping the prompt one layer lower would make every leaf replay miss and report
"replay diverged" against a faithful record, which is the failure P8 exists to prevent rather than
to produce. The consequence is that the bare-statement solver is not a leftover: it is precisely
what replay wires, and the budget-conditioned solver in a replay is a defect, not a consistency
fix.

Sizing the ceiling means converting currency to tokens, which needs a price sheet, so it lives at
the provider edge rather than in the core — the same boundary that keeps the core free of the
network.

### Planner and Reducer are distinct agents

Not two calls to one instance. The Reducer must see what returned without inheriting the priors
that produced the split. The Planner is the error concentrator in the system: it does the hardest
reasoning with the least information, planning the split before knowing what the children will
find. Every downstream node inherits its mistakes.

### DAG, not tree

Sub-problems are content-addressed (§6), so identical sub-problems resolve to one call and a node
may have several parents. This converts divide-and-conquer into dynamic programming and is the
single cheapest structural improvement available.

**The execution is a DAG; the record is a tree. This is a real divergence, not a wording
slip.** Sharing is achieved by *not making the second call* — the cache serves it, or `dedupePlan`
collapses it before fanout — so no node ever ends up with two parents. `NodeOutcome` has
`Children []string` and a positional `NodeID` (`n0.1.2`), which cannot express one. A shared
sub-problem appears in the record as a **cache-hit leaf under each parent that asked for it**, with
`CacheHit: true` and zero cost on all but the first.

The economics are identical either way — one call, once — so nothing in §3 or §4 is affected. What is
affected is §8: reading the record, a shared sub-problem looks like *N* nodes that agree, not one
node reused. The `CacheHit` flag is what distinguishes them, and it is load-bearing for exactly this
reason (see also P7: a hit is not an independent sample). Collapsing the record into a true DAG
would need stable content-addressed node identity rather than positional identity, which would break
the pre-order replay ordering P8 depends on. Not worth it for a presentation concern; recorded so
nobody reads "DAG" and expects to find a second parent edge.

### Base case

A node stops recursing when any of:

1. no verifier is available for its children (P2), or
2. the planner declines to split (P1), or
3. its budget allocation falls below the cost of one solve plus one verification, or
4. `max_depth` backstop.

### Alternatives considered

Recorded so the choice is legible, not to reopen it:

- **Blackboard** (Hearsay-II): opportunistic, decomposition emerges rather than being predicted.
  Better when the split cannot be known in advance; worse for budget accounting, which is the
  point of this system. Rejected for v1, plausible for a later mode.
- **Portfolio**: N whole-problem attempts plus selection, rather than partition. Strictly better
  when selection is easier than generation. **Built** — see *Strategy* below. It is the natural
  fallback when P1 says don't decompose, and it is a first-class strategy the planner chooses.
- **MCTS-style allocation**: expand by expected marginal value rather than uniformly. This is the
  intended v2 of the ledger's apportionment policy (§3).
- **Speculative decomposition**: commit to a split, roll back the subtree when a later check
  invalidates the parent's plan. Deferred; needs cancellation (§10) first.

### Strategy: partition or portfolio

A plan declares its **strategy**. Partition is the zero value, so every planner written before
strategies existed still means what it meant.

| | Partition | Portfolio |
|---|---|---|
| Children are | different sub-problems | the same problem, attempted independently |
| Two identical children mean | redundant work — collapse them | the entire point — keep them |
| The reducer | **merges** | **selects** |
| Arms read the cache | yes | **no** |

**The strategy cannot be inferred from the items, and that is the whole reason it is declared.** The
two shapes assign *opposite* meanings to an identical child statement: under partition it is
redundancy to collapse (the DAG win above), under portfolio it is the independent replication the
strategy exists to buy. Nothing about the items distinguishes the cases — only intent does.

Three pieces of machinery were silently correct for partitions and silently wrong for arms. All three
were latent before portfolio existed:

1. **`dedupePlan` collapses same-key items.** Under portfolio that turns *N* attempts into one call
   and reports the run as a portfolio that happened. It now skips portfolios entirely.
2. **Apportionment was keyed by problem key.** A key is not unique across a plan, so a portfolio
   collapsed to a single allocation — every arm but one silently underfunded, with nothing in the
   record to say why the run degraded. Allocations are now returned **indexed by plan position**,
   which is unique by construction. (This was a defect for *partitions* too: a planner proposing the
   same sub-problem twice hit the same collapse.)
3. **Arms must not read the cache.** Arm 1 writes; if arms 2..N could read, they would be served a
   *copy* of arm 1, and the run would claim *N* independent attempts where one happened. This is
   precisely how §6 warns a cache "saves money by destroying replication", and P7 is explicit that a
   hit is not an independent sample. Arms still **write**, so the entry accumulates *N* genuine
   samples — which is what P7 wants and a single-answer cache denies. The suppression applies to
   *arms only*: a partition's children keep the DAG win, and the root is never an arm.

Selection is where the strategy earns its keep, and it is deliberately available at two prices. The
mechanical `SelectReducer` takes the first **verified** arm, else the first arm with content: free,
deterministic (so it underwrites replay, P8), and testable with no model. A portfolio whose arms are
individually verified turns selection into "take one that passed", which is exactly the case this
strategy is strictly better for. The model-backed selector is asked for an **index**, never for prose
— a selector allowed to rewrite would convert selection into generation, billing the run for a
synthesis nobody planned and returning an answer that matches no recorded node. Neither selector
copies the winning arm's cost or verdict forward: the arm already recorded both on its own node, so
carrying them up would double-count the spend and let the parent claim a check it never performed.

### Shapes not supported, and the line between them

The shapes above divide cleanly on one question: **do the children stay independent of each other?**

Portfolio and speculative decomposition keep them independent, which is why portfolio cost three
mechanical fixes and no redesign. Debate, sequential chains where one child consumes another's
answer, and blackboard all break independence — and independence is load-bearing in three separate
places: apportionment divides a fixed pool among children that cannot renegotiate (§3), DAG collapse
assumes an identical sub-problem has one answer regardless of who asked (§6), and replication treats
sibling draws as independent samples (§7). Any shape that breaks it is a redesign of §3, not a new
strategy — which is the honest reason blackboard is deferred rather than merely unbuilt.

---

## 3. The ledger

Budget is a **currency with an allocation policy**, not a counter with a limit.

- A run begins with a cap, denominated in a normalized unit rather than dollars where the
  deployment is a campus one (a PI holds an allocation; the question becomes a balance check
  rather than a forecast).
- A parent receives a balance and **apportions** to children — using the planner's relative
  weights (§2), normalized to the balance actually available.
- Children **return unspent balance** to the parent on completion.
- The global bound is emergent, not a cliff at a fixed depth.

### Reserve

A node never apportions its full balance to children. It withholds:

1. **The reducer's own cost.** Merging is not free. Allocating 100% to children and having
   nothing left to combine them is a real and embarrassing failure mode — the run pays for every
   sub-answer and cannot produce the answer.
2. **Retry headroom.** Verification failures cause re-solves (P2); a node with no reserve cannot
   act on its own verifier.
3. **Surplus for adversarial passes** if the subtree comes in under.

Working default: apportion ~60–70%, hold the rest. The fraction is a policy knob and an open
question (§12).

## 3.1 Denominations: money and time

Caps work in time as naturally as in money, and researchers already think this way — *an hour*,
*by Friday*. These are not two flavours of one constraint. They bound **different dimensions of
the tree**:

- **Spend ≈ total nodes** — dominated by *breadth*.
- **Latency ≈ depth × per-call latency** — the critical path, largely independent of breadth.

So a money cap constrains how wide the tree may be and a time cap constrains how deep. Together
they pin the tree's **aspect ratio** from two directions, which is the good/fast/cheap surface
made concrete and schedulable: wide-and-shallow satisfies a tight deadline with loose money;
narrow-and-deep satisfies tight money with loose time. Under P9 the planner is told both and
selects the shape that fits.

### The propagation duality

The two denominations propagate in mirror-image ways. Getting this backwards is an easy and
expensive bug:

|  | Across breadth (siblings) | Along depth (parent → child) |
|---|---|---|
| **Money** | **divides** — each child gets a share | inherited whole |
| **Time** | inherited whole — siblings share the window | **divides** — parent reserves time for its reduce |

The reserve policy above applies to both. A node that gives its children the full remaining window
has no time to merge their results — the same failure as spending the whole balance on children,
arriving by a different route.

### Two time regimes

**Latency cap ("one hour")** — the run must *complete* within a window. Forces parallelism,
constrains depth, and generally costs more.

**Due date ("by Friday")** — the run must be *done by* then. This is a scheduling constraint, not
a latency constraint, and it is the more interesting of the two because **slack is convertible
into money**. A run that is not needed for three days can go to batch inference, off-peak, or
deferred execution at a substantial discount to on-demand.

This is where "pick two" stops being a metaphor: giving up *fast* mechanically buys *cheap*. A
researcher who does not need an answer until Friday should pay materially less, and the interface
should tell them so — the deadline field is a price control, not just a scheduling field.

Deferred runs also sit well with P5: work waiting in a queue or table accrues no idle cost.

### The due date is reachable from outside the process, and buys nothing yet

Added by [#11](https://github.com/scttfrdmn/quarry/issues/11), which found that the paragraphs
above described a capability **no caller could reach**. `Caps` carried `Latency` *and* `Due`, and
`Deferrable()` had been written and tested — but `cmd/quarry` offered only `--deadline`, a relative
duration, so `Due` was never set by anything and `Deferrable()` never returned true outside its own
unit test. The price control was fully specified and structurally dark.

**A host's deadline is absolute, and the host resolves it.** `--due` (or `QUARRY_DUE`) takes an
RFC3339 instant. The host owns the clock: it knows when the request arrived and what it promised.
quarry must not call `time.Now()` in the root package, so a *relative* duration from a host would
have to be resolved against an instant quarry is not supposed to read — which is why the absolute
form is the one that crosses the boundary and the relative form stays the human's.

**A due date must not imply a latency cap.** `Deferrable()` is `Due` set *and* `Latency` zero, so a
resolver that helpfully derived one from the other would record a due date and silently price the
run as on-demand: it would look right and cost more. The run is bound by *when*, not by *how long*.

**Nothing prices off `Deferrable()` yet, and this is the honest statement of that.** `Due` has one
real consumer — `RootContext`, which takes the earlier of `Latency` and `Due` as the context
deadline — so `--due` genuinely bounds a run. What it does not yet do is buy anything cheaper: the
discount needs a provider that can offer batch or off-peak, and this section names ember as the
executor for that mode rather than a component built here. #11 makes the denomination **reachable**;
the discount is a separate build. A doc that claimed otherwise would be describing the flag's
motivation as its effect.

An **expired** due date is accepted, not refused. It is not malformed — a host that queued a request
reaches it by ordinary delay — and this section's own miss semantics say whatever exists must be
returnable *now*. The faithful outcome is a truncated record whose gaps are named, not a refusal
that produces no artifact at all.

### Executor

Deadline-denominated runs are precisely the workload
[ember](#) was specified for — per-prompt deadlines, maximize per-round batch size, minimize
deadline misses, sliding per-prompt cost with *now* most expensive and decaying toward zero. quarry
generates an unusually good batching population for it: sibling nodes at the same tree level share
a model, a prompt shape, a deadline, and mutual independence. ember is the executor for this mode
rather than a new component to build.

### Miss semantics differ

Budget exhaustion and deadline expiry are not symmetric:

- **Budget exhausted** — stop, return a degraded answer with gaps marked.
- **Deadline reached** — there is no option to return later. Whatever exists must be returnable
  *now*.

The second imposes a real requirement: the reducer must be able to run over partially-returned
children at any moment. The tree must hold a returnable answer at all times, degrading in quality
rather than existing in an unreturnable intermediate state. This pushes §12's failure-semantics
question decisively toward *degraded answer with gaps marked*.

The ledger rides with the request the way trace context does — it is baggage, not a global. It
carries: remaining balance, depth, lineage, deadline, and the `agate:` scope tags (P6).

### Admission control

Every model call passes the check: *does this node's balance cover this call?* In the integrated
deployment this is the existing agate chokepoint, which already does exact pre-call caps and
server-enforced metering. No new mechanism. The standalone build implements the same interface
in-process so the two are swappable.

### Decorator ordering (spec, not preference)

```
Budget(Retry(agent))     # correct — retries consume budget
Retry(Budget(agent))     # wrong — retries are free, cap is not a cap
```

This must be enforced or linted, not documented and hoped for.

### Surplus

A run that completes under cap spends the remainder on adversarial passes over the
highest-exposure claims (§5). Budget converts to quality rather than evaporating. This is active
work inside the authorized ceiling and is consistent with P5.

---

## 4. Cost estimation

Ordered by what they cost to produce.

**Everything in this section is advisory.** Under P9 the planner fits a plan to the stated cap,
so no mechanism here is on the critical path — a bad estimate produces a *worse-scoped* run, not a
truncated one. Read this section as decision support for the researcher choosing a cap, not as a
component the system depends on.

### Structural ceiling — free, guaranteed

With max branching `b`, max depth `D`, per-node cost `c`: total ≤ `c·(b^(D+1) − 1)/(b − 1)`.
Wildly pessimistic and always true. Quote it as the cap, not the estimate.

### Probe — one call, ~1/N of the run

Run only the top-level Planner. Require it to emit the split *and* tag each child leaf-or-recurse
with a difficulty score. This yields a measured branching factor at depth 1, and is the highest
information-per-dollar action available.

### Branching-process projection

With mean offspring `m` from the probe, expected node count follows Galton-Watson: converges to
`1/(1−m)` for `m < 1`, diverges for `m > 1`.

**Surface this rather than hiding it.** Near `m = 1` the variance dominates the mean and any
single number quoted is theatre. The UI should say so.

Split the estimate: input tokens are largely the halo and computable from the plan without
running anything; output tokens are the stochastic part. The predictable half must not inherit
the uncertainty of the other half.

### Report

Three numbers, never one: **P50, P90, and the structural ceiling.**

And the estimate stays advisory *even at the approval gate* (§2), which is where the temptation to
promote it is strongest. A plan artifact is the one place a host is looking at a projected cost while
deciding whether to spend, so it would be natural to refuse a plan whose P90 exceeds the cap — a
constraint derived from an estimator §13 calls weak, and P4 forbids it. What the artifact carries is
three numbers plus a sentence naming which of them is trustworthy in this regime: at or above a
branching factor of 1 the projection diverges and only the ceiling means anything. Nothing gates on
any of them. **The cap does the refusing**, mechanically, on the apportionment — which needs no
estimator at all.

### Plan-variance diagnostic

Sample the Planner k times (k≈5) and measure divergence between plans. High divergence means both
that the estimate is unreliable and that the problem is underspecified — which the researcher
wants to know before spending anything. This same probe serves two further purposes (§7); it is
paid for once.

### Calibration

Every run deposits `(problem shape, plan shape) → actual spend` into a corpus, keyed by the same
content hash as the cache. Nearest-neighbour over accumulated runs beats any a-priori model
quickly. This is the SLURM walltime situation: user estimates are poor, historical
per-application data is decent, prefer the latter and allow override.

---

## 5. Verification

**"Good" is defined as verified.** This is what makes the third axis of good/fast/cheap
specifiable at all — without a verifier there are two sliders wearing a third's clothing.

### Verification is a second budget

Quality becomes expressible as a **ratio** even where quality itself is not directly measurable:
~10% overhead is a sanity check, ~50% is adversarial review at every node, >100% means every
claim independently re-derived by something trying to break it.

The verify:generate cost ratio is domain-determined — and it is the *same variable* that decides
whether decomposition works at all (P1, P2). One parameter, not two. The NP intuition that
checking is cheaper than finding does not transfer to natural-language claims.

### Ladder, cheapest first

| Mechanism | Cost | Catches | Blind to |
|---|---|---|---|
| Mechanical oracle (compiler, tests, residual, schema) | ~0 | Hard failure | Anything unformalized |
| Redundancy / self-consistency, N× | N× | Variance | Systematic bias |
| Judge | fraction of generation | Obvious failure | Its own family's errors |
| Adversarial | high | Specific defects; asymmetric — needs one hit | Unknown unknowns |
| Debate + arbiter | highest | Judgement-laden claims | Cost |

**Judge independence is a requirement, not a nicety.** Same-family judging correlates errors.
Route judges through a different provider — one of the better arguments for agenkit's six
adapters.

### Where the regress stops

At a mechanical oracle, at a human, or at a stated residual risk. It must terminate explicitly
and the receipt must say where.

---

## 6. The cache

Content-addressed sub-problem memoization. Permitted under P5 as productive per-byte storage;
DynamoDB on-demand + S3, never a provisioned cluster.

### Key = hash(sub-problem) + scope tag set

**Not the hash alone.** Content-addressing feels identity-neutral and here it is not: two users
can pose a hash-identical sub-problem while holding different entitlements, and one's cached
answer may be derived from documents the other cannot see. Serving it across that line walks
straight through the ABAC boundary (P6).

This forfeits most cross-user reuse. Within a run and within one user's history — where the bulk
of duplication lives — it still pays. Genuine cross-tenant reuse is sound only for steps provably
derived from nothing scoped (pure reasoning, no retrieval), which is a separate opt-in class and
not the default.

### Entries accumulate samples, not a single answer

A cache entry holds **every result produced for that (sub-problem, scope) key**, not the first one.
A hit returns the distribution; a fresh run appends to it.

This resolves what would otherwise be a direct conflict between §6 and §7: a cache that returns a
stored answer saves money and destroys replication, because the second run is no longer an
independent sample. Accumulating instead means repeated runs *increase n* — the cache becomes the
sample store that P7 requires, and re-running a question tightens its error bars rather than
echoing the first result back.

Read policy therefore has two modes: **serve** (return the existing distribution, spend nothing)
and **extend** (draw a fresh sample and append). Nodes flagged unstable are always extended; §8.1
governs the choice.

### Invalidation

Entries carry the document versions they depended on and expire when those change. Otherwise a
re-ingest silently serves stale sub-results. The lineage recorded for the receipt is what makes
this possible.

### Only complete results are cacheable

A cache entry has no way to express incompleteness. A served hit copies the content and flags the
node a hit; nothing restores a gap, and nothing says "this answer rests on two of its four
children". So a partial result, once stored, is laundered into a complete one for every later
reader.

That matters most for the operation that depends on the cache being right: `extend` (§8.1) exists
to refill what truncation left empty, and it prices its delta by serving the *completed* subtrees.
If an incomplete merge were cacheable, the node most in need of a re-solve would be the one most
confidently served, and an extend would reliably refill nothing while reporting a cache hit and a
bill. The implementation had this defect on the internal-node path — leaves guarded on
completeness, merges did not — and the failing case is exactly the one above.

Note that "incomplete" is not visible from the outcome alone, which is why the reducing node passes
it explicitly. A merge over one of two children has real content, and because only time produces a
gap (§3.1) it carries no gap flag either. It looks complete from every field it has.

### Retention

TTL on all entries, so the idle floor does not creep upward run after run.

**The store's clock is the store's own**, not the sample's. A `Sample` carries a provenance
timestamp stamped by whoever made the model call, and it is legitimately absent for any solver that
does not stamp one — the core is forbidden to read a clock, so most samples in a no-provider build
have none. Deriving expiry from that field made every unstamped sample *born expired*: with any
non-zero TTL the cache served nothing it had ever written, while still counting those samples in
its sample count. A store that reports holding what it will not return is worse than one that holds
nothing, because the first looks like it is working. Every method that can observe expiry therefore
takes the current time from its caller, including the sample count — the count and the read must
agree about what the store holds.

---

## 7. Reproducibility and replication

Treat these as separate properties. **The pipeline is a computation; the model is an instrument.**
Computations get determinism, instruments get error bars. This system is unusually well placed to
enforce the distinction because the tree is explicit rather than buried inside one generation.

Using NASEM vocabulary, since this will meet a research audience:

| Level | Claim | Achievable? |
|---|---|---|
| **Record** | Full transcript, model IDs + versions, params, document versions, tree, ledger | Yes — required |
| **Reproduce** (replay) | Re-execute the tree against recorded responses; no model calls | Yes — deterministic by construction |
| **Replicate** (re-derive) | Fresh run, same inputs, consistent *conclusions* | Yes — measurable, with variance |
| **Bitwise** | Identical tokens | No. Batch composition is not controllable through a hosted endpoint. State this as a limitation rather than hoping. |

For an instrument with irreducible variance, **replicability is the load-bearing scientific
claim**; reproducibility is bookkeeping hygiene.

Keep replay anyway: it is the control that makes replication interpretable. When two runs disagree
and you cannot replay, you cannot tell whether the model or your merge logic caused it. Pinning
the deterministic half is what makes the stochastic half's variance attributable — the same reason
you calibrate an instrument before trusting its scatter.

### Replay substitutes three seams, not one

Once the planner and reducer are model calls, **there are three stochastic seams and a replay must
pin all of them**:

| Seam | Replaced by | Replays |
|---|---|---|
| `Planner` | `PinnedPlanner` (`PinPlan`) | the recorded decomposition |
| `Solver` | `RecordedProvider` | recorded leaf samples |
| `Reducer` | `RecordedReducer` | recorded merge/selection output |

This was originally a one-seam design, and the reason is worth recording: while the planner and
reducer were deterministic doubles, substituting only the provider *was* a complete replay, and the
determinism test passed. It kept passing after they became model calls — while silently no longer
covering the two most important nodes in the tree. A test that measures nothing is worse than an
absent one, so the determinism test now runs a planner *and* reducer that return different output on
every call, with a non-vacuity guard asserting they really drift.

The reducer needs its own seam rather than being folded into `RecordedProvider`, because a reduce
call reaches the provider with the **merge prompt**, not the problem statement — so the provider's
replay key `(problem, scope, model)` can never match it. `RecordedReducer` keys on the node's
**position** `(depth, problem)` instead. Depth is part of the key for the same reason it is part of
`PinPlan`'s: a portfolio's arms share their parent's problem key by definition, so key-only indexing
let a leaf arm overwrite the internal node above it.

A miss at either seam is an **error, never a fallback**. Being asked for a call the record does not
contain means the replay produced a different tree, which is real information about the pinned plan;
folding the children live instead would hide it behind an answer that looks fine. `Replayable(record)`
builds all three together so a caller cannot wire a partial replay — the failure mode of which is
live model calls, real money, and a tree that will not match.

### A partial record must replay as partial

**Found by running the binary, and it made replay unavailable for the records most worth
interrogating.** `quarry replay` failed on *every* record containing a gap. Since §3.1 makes a partial
run the normal outcome under a deadline, that is most of them.

Three distinct causes, all the same mistake — a partial run treated as a broken record rather than a
faithful one:

1. **Gaps were skipped when indexing.** A gapped node made no model call, so it has no sample — but
   the replay still *visits* it, because the pinned plan reproduces the shape. So the lookup missed,
   and the miss was indistinguishable from a genuine divergence. "This node was cut short" and "this
   node is not in the record at all" are opposite claims about the same failed lookup, and they need
   separate signals: gaps are now indexed separately and a hit returns `ErrRecordedGap`, which the
   executor's existing time-miss path turns back into a gap. An unrecorded prompt still reports a
   divergence, and a test asserts the two do not collapse — if they did, replay would stop being able
   to detect that the tree changed shape, which is the only thing it is for.
   - A gap is keyed **without the model**, unlike a sample: a node that never reached a provider has
     no model recorded. It is still scope-qualified, so P6 holds — a gap cannot be served across an
     entitlement boundary any more than an answer can.
2. **`BoundBy` was recomputed on replay.** It reads the live execution environment, and a replay runs
   with no deadline on purpose — so a run the clock bound recorded `latency` and its replay recorded
   `""`. `ReplayRecord` now inherits every field a replay cannot legitimately re-derive (`Problem`,
   `Caps`, `Mode`, `BoundBy`, `Adversarial`) and re-derives only the tree, the unverified list, and
   the hash. The split is the content of the change: re-deriving a field from an environment the
   replay deliberately does not reproduce guarantees a false divergence.
3. **An all-gap record was refused outright** — "there is nothing to replay", because no node named a
   model. It is the most time-bound run the system can produce, its gap index is complete, and it
   replays byte-identically.
   - **And once more, later, at the same line.** Widened to excuse gaps, the guard still refused a
     record whose every node was *unfunded* — `--cap 0.000001`, one below-floor root. Widening a
     too-strict check by exactly the case in front of you leaves it too strict by every case that is
     not; see the accessor that ended it, below.

The pattern is worth naming because it recurred **four times** within one file's neighbourhood, twice
at the same guard: **the happy path is a complete tree, and every seam that assumed one broke on the
partial case in a way that reported the *record* as at fault.** Partial tolerance is not only the
executor's problem (§3.1); it propagates all the way to the artifact and its replay.

#### A fact of execution cannot be re-derived from the tree's geometry

`BoundBy` (cause 2) turned out to be an instance, not a one-off, and the **first live Bedrock run
produced the second of three**. `quarry replay` reconstructs the executor from the record, and it was
inferring the depth bound as *deepest recorded node + 1*. That is a **lower bound on the cap, not the
cap** — the two coincide only when nothing actually hit it. A `--depth 2` run produced 22 leaves all
recording `max_depth`; replay got a limit of 3, so those nodes were no longer *at* the bound, called
the pinned planner, and came back `planner_declined`. Twenty-two `BaseCase` fields differed, and
replay reported a divergence against a faithful record.

The fix reads the bound from the record, where it is stated: a node that stopped at `max_depth` names
the limit exactly. The inference survives only as a fallback for records where nothing hit the bound —
there the value is genuinely unobservable, and any limit at least that deep reproduces the same tree.

Every `--fake` record replayed clean throughout, and the reason is worth recording: the fake planner
declines on clause length long before it reaches a depth limit, so **no `--fake` record contains a
`max_depth` leaf at all.** The fixture could not construct the state the derivation got wrong.

Then a `--fake` sweep produced the **third instance**, and three is where a pattern stops being
bug reports and becomes a rule. `--cap 0.0001` recorded `below_floor` at the root; replay set no
`Floor`, and with no floor zero is never below it, so the root planned instead and recorded
`planner_declined`. The floor is worse than the depth bound, because the depth bound is at least
*sometimes* visible in the tree — a node that hit it says so — whereas **the floor leaves no trace
at all** unless a node happened to fall under it.

That is what moved the answer out of the CLI and into the record. `RunBounds` states the depth
bound, the floor and the retry budget, and `quarry replay` reads them rather than deriving a third
thing:

> **A fact of the original execution cannot be re-derived from the tree's geometry.** Every knob the
> executor was configured with is such a fact, and a record that does not state one is not
> self-sufficient (P8) — a replay must then guess, and a guess that is usually right is worse than an
> absent field, because it fails only on the runs where the knob mattered.

The resolved value is recorded, not the raw field: an executor left at `MaxDepth: 0` runs under
`DefaultMaxDepth`, so writing the zero would record a bound that was never in force. And a replayed
record **inherits** `Bounds` rather than re-deriving them, for the reason `BoundBy` does: they are
what the replay was configured *from*, so re-deriving them would agree by construction and prove
nothing.

The inference survives for records written before the field existed. Both branches stay exercised,
which is the honest state: one reads a stated fact, the other admits a guess.

#### The same defect wearing the other cap: unfunded is not gapped

**Found by the first live Bedrock run**, which is the point of the entry below. A 28-node run under a
$0.25 cap left four nodes *unfunded* — empty content, no model, and **no `Gap` flag**, because under
the standing ruling only time produces a gap — and `quarry replay` failed with "no recorded sample".
Identical symptom to cause 1 above, one category over, and the more common half: spend is the cap
researchers actually set.

Unfunded nodes are now indexed like gaps, keyed without the model for the same reason, and served as
`ErrRecordedUnfunded` — a **separate sentinel**, not a reuse of `ErrRecordedGap`. Reusing it would
have relabelled spend degradation as time truncation, which is the one distinction §3.1's ruling
turns on: the executor's gap path sets `Gap`, so the replayed record would have reported more time
pressure than the run experienced, and `Extend` would then offer a deadline raise where money was
needed (§8.1). The failing test shows the knock-on precisely — the flip also corrupted `Unverified`,
since gaps are excluded from that list, so one mislabelled category silently shortened the record's
statement of what was never checked.

The discriminator is **`Model == ""` together with empty content and no verdict**. Content-emptiness
alone will not do: an empty *answer* is a result, and conflating the two is the distinction §8 exists
to preserve.

**Why `--fake` could not find this, and why that generalizes.** The fake provider's per-call cost is
uniform, so the planner's affordability check either funds every child or declines the split — a tree
with *some* children priced out is not reachable. It takes a real price sheet, where one sub-question
costs several times another, to produce one. The fake is not merely cheaper than the live path; **its
cost structure is uniform where the real one is not, and a whole category of tree shape lives only on
the far side of that difference.**

**The same discriminator, derived three times, and the third copy had it wrong.** `quarry replay`
guards against a record with nothing to reproduce, and the guard read *"names no leaf model and has
no gaps"*. Under the standing ruling an unfunded node has no gap, so `--cap 0.000001` — a single
below-floor root, no model, no gaps — was refused as unreplayable. It replays byte-identically. The
guard had already been widened once for the all-gap case; widening it for gaps and forgetting spend
is the same omission the sentinel above exists to prevent, one layer up.

The bar for replayability is deliberately low, and the two claims it confuses are worth separating:
**"no model was called" is not "nothing happened."** Only the second makes a record unreplayable, and
it has exactly one cause — every node served from cache, so nothing was ever computed.

The fix is an accessor. `RunRecord.Unfunded()` states the test once, and the three call sites that had
each derived it — the replay index, `Truncated`, and this guard — now share it. A predicate subtle
enough to be got wrong (no model **and** no content **and** no verdict, since an empty *answer* is a
result) should exist in one place; three copies of it drifted, and drift is what a shared accessor
makes impossible rather than merely unlikely.

### Replicate independence — same ladder as §5

Resample same model → paraphrase prompt → different model family → different decomposition. The
last is closest to "different team, different setup" and is the strongest test. Judges,
adversarial passes and replicates are **one mechanism serving two epistemic functions** — one
budget, not two systems.

### Plan pinning

Offer freezing the recorded decomposition and re-running only the leaves. This attributes spread
to planning versus solving instead of reporting one undifferentiated number. Cheap to build,
genuinely useful as an experimental control.

**A control that changes the thing it controls for is worse than none.** This was learned twice, the
same way both times, and the second time was predicted by the first:

- **Strategy.** Pinning a portfolio run originally lost the strategy, so `dedupePlan` collapsed three
  identical arms into one child: the re-run did a third of the work and reported a faithful shape
  replay. Fixed by recording `Strategy` on `NodeOutcome` and keying pinned nodes by `(depth, problem)`.
- **Weight.** `NodeOutcome` did not record the planner's relative weights, so a pinned re-run
  reproduced the recorded shape and then apportioned uniformly across it. A 6/3/1 split came back as
  a third each — the shape faithful, the money not — so spread between run and re-run could come from
  the re-division rather than from the solving pinning exists to isolate. Fixed by recording
  `PlanWeight` on the child the weight funded (a plan's weights are observable only through the nodes
  they paid for), and it is the POST-dedupe weight: when identical children collapse their weights
  sum, and the sum is what actually funded the node.

The general hazard, which is the durable content here: **every field the pinned planner fails to carry
becomes a silent difference between the run and its own control**, and it differs in the direction
that still looks like a faithful replay. Zero `PlanWeight` therefore means *unrecorded*, not
weightless — a plan item's weight is always positive, so zero cannot be a real one — and an
unweighted record falls back to uniform **wholesale per node**, never per missing entry: substituting
1 for only the gaps would change the ratio between the recorded weights, and a wrong apportionment
presented as a pinned one is worse than an honest uniform split. The fallback is disclosed in
`Plan.Reasoning`, because the difference between a full control and a shape-only one has to be legible
to whoever compares the two runs.

What pinning still does **not** control is the solver — children are re-solved live, which is the
entire point. `ExpectLeaf` and `Rationale` are also unpinned, deliberately: neither reaches
`Apportion` or the tree shape, so neither can change what the re-run does.

**Pinning is not the approval gate, and cannot become it.** The two look alike from a distance — both
replay a decomposition somebody else chose — and the difference is the direction they face in time. A
pinned planner is built *from a finished run record*: it reads each child's weight off the child the
weight funded, which only exists because the money was already spent. The gate (§2) has to answer
*before* anything is spent, from a plan and a balance, with no outcomes to read. So the artifact
carries its own weights and its own apportionment, and the two mechanisms stay separate: pinning is
an experimental control over spread, the gate is an authorization over spend. Collapsing them would
mean either a control that needs prior authorization or a gate that needs a prior run.

### Claim-level equivalence — no longer unbuilt, and what building it exposed

This heading read "**the hard unbuilt piece**" and the body said "**Prototype this first** — everything
else in this section assumes it." The prototype has landed, so the heading is corrected; the sentence
that survives intact is the reason: *to claim runs agree you must compare conclusions, not text.*
Without this, "agreement" is vibes with a percentage attached.

**A comparison is a metered, three-state, cancellable judgement — not a predicate.** The original
sketch assumed `Equivalent(a, b) bool`, and every part of that signature forbids what a semantic
comparator has to be. No `ctx`: a model call must be deadline-bound (§3.1), or the comparison outlives
the run it reports on. No third state: a bool cannot say *I could not tell*, so a refusal, a timeout
or an exhausted budget would be reported as **disagreement** — the §8 defect of converting silence
into a finding, here converting a billing event into a scientific result. No cost: an O(n²) sweep of
paid calls is the easiest way to spend a lot of money, and P4 says a seam that cannot report its spend
cannot be capped. So the bool relation stays as the free rung and the model arrives behind a wider
seam (`ClaimComparator`); `StabilityReport` gained `Unassessed`, `ComparisonCost` and `Truncated` to
carry what the wider seam can now say.

**The soundness asymmetry is what makes it affordable.** Normalized equality is a *sound* sufficient
condition — equal norms mean the same claim, and no model is needed to confirm it — and merely
*incomplete*, missing paraphrase. So the free rung pre-clusters exactly, for nothing, and paid calls
run only **between distinct wordings**: n claims collapse to k canonical forms, and the paid work is
O(k²) over wordings rather than O(n²) over claims. Replicates of a near-deterministic pipeline
collapse almost completely. This is what `Claim.Norm` was pinned into the record for. A free match
therefore **never** escalates: there is nothing a paid call could add, so every micro-unit it spent
would be waste.

**Clustering was order-dependent, and it was found by probing before the comparator existed.** The
implementation compared each claim only against a cluster's *representative* and joined the first
match — single-link clustering, sound only if equivalence is transitive. Normalized-string equality is
transitive, so no test could see it; **no model-backed comparator ever will be**. With a~b, b~c,
a!~c, three replicates asserting a, b and c reported *2 clusters, nothing stable* in one order and
**1 cluster, unanimous and stable** in another — same replicates, same comparator, opposite scientific
conclusion decided by iteration order, and the error in the direction that reports agreement no
comparator asserted. Replicates are exchangeable draws (P7); their order must carry no information.
Two changes fix it: **complete link** (a claim joins only if equivalent to *every* member, which under
a transitive relation is identical to single link and under a non-transitive one cannot chain), and a
**canonical pre-pass** (group by pinned norm, then walk groups sorted by norm, so the comparator sees
the same pairs in the same sequence regardless of input order). Complete link is also the conservative
choice, which is the direction this section requires: it splits borderline clusters and so undercounts.

**Admission has to precede the call, and a live run is what proved it did not.** `ClaimComparator`
carries an `Estimate(a, b)` alongside `Compare`, for the same reason `Adversary` and `Provider` do: a
comparator that cannot be *sized* cannot be *admitted*, and a caller left to spend first and ask
permission afterwards is not doing admission control at all. The first implementation debited after
calling, and a live run against a 1-micro-unit cap **spent 200 while reporting `Truncated` perfectly
honestly**. The distinction that matters: *disclosing an overrun is not the same as not overrunning.*
P4 makes the cap the contract rather than a target, so a test that asserts only "it said it was
truncated" cannot see this — the assertion has to be about the spend. A comparator may over-estimate;
that refuses an affordable comparison rather than authorizing an unaffordable one, which is the safe
direction (§4).

**Order-symmetry has to reach the model too.** Equivalence is symmetric; a model asked "does A say the
same as B" is not, because the first claim frames the comparison. The pair is therefore canonicalized
before the prompt is built — smaller normalized form first, always — so both argument orders send an
identical prompt. Without that, the clustering guarantee stops at the seam.

**§5's family-independence requirement does not apply here, deliberately.** An adversary must be a
different model family because it *judges* the solver's output and same-family error correlation makes
the pass theatre. A comparator judges no quality — it asks whether two strings assert the same thing —
so family correlation has no mechanism to bias it. Recorded as a known non-requirement rather than an
unenforced one.

It still shares the extraction the citation receipt needs.

### Report instability, not agreement

The valuable output is the list of **unstable claims** — where the instrument is at its limit.
More honest and more useful than an agreement rate.

### And some agreement rates must not be reported at all

The rate is a courtesy alongside the list, so there are cases where the courtesy is a lie. A
comparator that can abstain makes three of them, and all three render as `0.0`:

| Case | What the float says | What is true |
|---|---|---|
| n=1 | nothing replicated | there is no estimate (P7) |
| rate 0, some pairs **unassessed** | nothing replicated | nobody could tell |
| the comparison pass **truncated** | a measurement | admittedly under-merged, so provisional |

The second is the one that had to be found by probing rather than by reading: the comparator seam
keeps "I could not tell" distinct from "different" (that is why `Compare` returns `ok`), the report
keeps it distinct again (`Unassessed` is its own count), and then the projection to a front end
divides two integers and the distinction is gone. **A three-state distinction can be flattened at
the last hop even when every layer before it honours it** — which generalizes the §8 rule against
converting silence into a finding: it is not enough for the finding-producer to be careful.

The suppression rule is **narrow on purpose**, and the narrowness costs more thought than the rule.
Under the free comparator nearly every multi-replicate report has unassessed pairs, so suppressing
on that alone would omit nearly every trust summary — and with it the verified/unverified counts,
which were never in doubt. So a **floor above zero is still published**, labelled a floor: a lower
bound with real support is information. And a rate of 0 reached by asking about *every* pair is
published too — that is the strongest instability signal the system produces, and hiding it behind
the flag that exists to prevent overclaiming would be the same error in the other direction.

What a rate cannot survive is arriving as a bare number. `ComparedBy` exists because a floor from
string equality and a measurement from a model are the same float otherwise (P8), and a front end
whose wire shape has no field for either qualifier can only be told the honest thing by being told
nothing — see §9.

---

## 8. The run record

The citable artifact. Content-hashed, self-sufficient, replayable without model access (P8).

Contains: the answer; per-claim provenance to originating node and source documents with
versions; the cost receipt (extending agate's existing per-answer receipt); the verification
receipt — what was checked, by which oracle, pass rates, **and what remains unverified**; the
stability estimate with unstable claims flagged; the full tree and ledger; model IDs and versions;
the point at which the verification regress terminated.

**A receipt that does not add up is worse than no receipt**, so the rule for reading one is part
of the contract and not left to the reader. The wire carries USD floats because agate prices in
dollars, but **reconciliation happens in micro-units**: convert each row with `round(cost × 1e6)`,
sum as integers, compare. Summing the floats does not work — each row is converted independently
while the total is summed integrally first, so the error grows with the row count and no fixed
tolerance is correct. Truncation in place of rounding is equally wrong, for the reason the
chokepoint seam already documents: it loses the last micro-unit.

The floats are still exact **per row** — each maps 1:1 back to its `Units` value — which is why
this is a rule about summation rather than a defect in the projection. Learned late: the test
asserting this invariant passed for the life of the file because its fixture summed `1 + 2 + 3`,
which is exact in binary floating point (quarry#18, surfaced by a host integrator asking for a
non-summing receipt as a *malformed-input* fixture — it is what an ordinary run emits).

## 8.1 Iteration: extend and refine

A first run is usually a first stab. Because sub-problems are memoized (§6), a researcher can come
back later with more budget and pay only the **delta** rather than the total — which is the
economic justification for the cache's complexity.

There are two distinct operations, and the run record already says which one applies:

**`extend`** — the tree was *truncated* (cap reached, gaps marked). The plan was sound; the money
ran out. Same decomposition, more budget, fill the gaps. Completed subtrees serve from cache at
no cost.

**`refine`** — the tree *completed* but the result is too coarse. Here the plan was the problem,
and extending it would spend new budget deepening a decomposition that was split in the wrong
places. Instead the planner **re-plans from scratch**, with the prior run supplied as evidence
rather than as a starting point.

The distinction is enforced, not documented. An `extend` carries a pinned planner and no evidence;
a `refine` carries evidence and no planner. That asymmetry *is* the difference between the two
operations, so it is structural: a refine shipping a pinned plan would be an extend wearing the
wrong label, and extending a completed run is refused outright, because a pinned plan handed more
money serves every node from cache and pays for a copy of its predecessor.

Two things the implementation forced, both about deciding *which* operation applies:

**Truncation is broader than gaps.** Only time produces a gap (§3.1), so a run that hit its spend
cap and dropped half its children has no gaps at all — while being the clearest possible case for
extend. Since spend is the cap researchers actually set, deciding on gaps alone would route the
common case to refine and re-plan a decomposition that was never given the money to prove itself.

**The cap must rise in the denomination that bound the prior run.** More money will not refill a
node that ran out of time. An extend that raises the wrong ceiling re-derives the same truncation,
so it is refused rather than run.

That refusal depends on the record knowing which cap bit, and **for a while it did not.** `BoundBy`
was read from the root context at the end of a run — which answers "is a cap binding *right now*",
not "which cap bit *during* this run". Those differ structurally, not occasionally: `ChildContext`
withholds a time reserve so the reducer can merge (§3.1), so a child's window is strictly shorter
than its parent's and the root's is the whole cap. A deadline that truncates the entire fanout
therefore leaves the root's context unexpired. A four-node run that gapped **every** node recorded no
binding cap at all.

The consequence was not cosmetic. With `BoundBy` empty, the extend check falls to its unknown-binding
branch and accepts *any* raise — so a purely time-truncated run accepted a spend raise that could not
refill a single gapped node, which is precisely the mistake this paragraph exists to prevent,
arriving through the front door. It is now derived from the tree as well as the context: a gap
anywhere is proof the clock bit somewhere, since only time produces a gap.

Spend deliberately gets **no** equivalent fallback. An unfunded node is planned degradation with
budget remaining, so inferring "bound by spend" from it would report a cap as binding when the run
simply declined to spend more on that branch. `Truncated()` already catches that case by its own
third signal.

Worth recording *why* this survived: the test guarding the invariant assigned `BoundBy` by hand
before calling extend. It proved extend read the field correctly while proving nothing about whether
any real run ever set it. A test that constructs the state it means to detect cannot discover that
nothing produces it.

### Extend is the cheap half, and it is cheap because it is composed

`extend` needs almost no new machinery: plan pinning (§7) already *is* "the prior decomposition",
and the cache's serve path already *is* "completed subtrees cost nothing". Extend is those two
facts pointed at a larger cap.

Its correctness therefore rests entirely on the cache never having stored an incomplete answer —
see §6. It also rests on something the record cannot enforce: **the caller must supply the prior
run's cache.** Without it every completed subtree is re-solved at full price, which turns the delta
into the total while still producing a correct answer. That failure is silent and surfaces only as
a bill.

### What the planner sees on refine

Not the prior transcript — that is the halo problem (P1) and is expensive. A distilled prior:

- **Actual difficulty versus its own prior weights.** Which children turned out expensive and
  which were trivial. This is a direct correction on the planner's weighting (§2), operating
  within one research question rather than across the calibration corpus (§4).
- **Which claims came back unstable** (§7). This is the highest-value targeting signal in the
  system: the first run measures where the instrument is at its limit, and the second run spends
  precisely there.
- **Which nodes failed verification**, and where the regress terminated at a stated residual risk.
- Any gaps marked from truncation.

So a refine is not "the same thing but bigger". It is **budget targeted at measured instability**,
which is what makes successive runs compound rather than merely repeat.

### Cross-run accounting

Spend is tracked above the run: a **line of inquiry** carries cumulative cost across its runs, and
each run debits it. A PI cares what a *question* cost, not what an invocation cost.

Provenance chains likewise. A refine run's record references its predecessor's hash, so the
citable artifact (P8) may be a lineage rather than a single record.

The first signal is the reason nodes record the relative weight their parent's plan funded them by.
Without it, "this node was expensive" is a number; with it, "this node was expensive *relative to
what the planner expected it to cost*" is a correction the planner can act on. A node weighted 1
that consumed half the budget is the planner's own mistake made legible.

The distillate is a **filter, not a reformat**. A node that cost nothing, passed its check and came
back stable is not evidence — it is halo (P1), and the planner pays in context for everything it is
shown. Only the unstable claims travel, for the same reason: the stable ones are precisely the part
a refine need not revisit.

Note what the second signal requires. Instability is a *cross-replicate* measurement (§7), so a
refine after a single run has none — and no stability data must not be allowed to read as "every
claim was stable", which is the reading that would send a refine looking for nothing to fix. An
absent measurement is absent, not a clean bill of health. The difficulty actuals still arrive, so a
refine after one run is informed, just not targeted.

### The anchoring tension — the shape is withheld

Showing the planner the prior decomposition biases it toward that decomposition, which partly
defeats the point of re-planning. Worse, §7 names *different decomposition* as the strongest
replication test; if refine always anchors on the prior plan, iteration quietly destroys the best
independence signal available.

Of the two candidate mitigations, the implementation takes the first: **supply findings and
difficulty actuals, withhold the tree shape.** Concretely the distillate carries no children, no
node IDs, no depths and no strategies — node IDs matter as much as the rest, since they reconstruct
the tree by prefix. What survives is per-node evidence about difficulty and reliability, which is
what this section asks the planner to correct on and which does not describe a shape.

The cost of that choice, stated rather than hidden: the planner cannot tell that two distilled nodes
were siblings, so it can learn "this sub-problem was expensive and unreliable" but not "this split
was wrong". That is strictly less than the ideal above. It is the price of not biasing the shape, and
it is paid deliberately, because a biased plan is *undetectable* in the record while a missing signal
is merely absent.

The second mitigation — sample plans both with and without the prior and compare — is not
foreclosed. It needs a planner-level experiment rather than a distillation rule, and a caller
wanting the anchored arm can assemble the prior from the record directly. So the *tension* is
resolved in the only place it can be resolved cheaply; the *experiment* that would settle which arm
is better remains open, and stays flagged in §12.

## 8.2 Telemetry and self-calibration

The system should learn from its own operation — which decompositions work, what verification
actually costs by domain, where tokens go. **The instrumentation for this is already required by
P8.** Run records exist for provenance; the learning layer is aggregation over records that must
be kept anyway. It costs almost nothing to build and should be on from run one.

Separate three activities with very different risk profiles:

1. **Measurement** — record it. Cheap, safe, unconditionally right.
2. **Learning** — derive priors from the record. Safe while the priors stay advisory.
3. **Optimization** — let the system change its behaviour from those priors. This is where it can
   go wrong, and it needs the guardrails below.

### The Goodhart problem, stated plainly

**A system tuned for efficiency will learn to produce cheaper answers, and cheaper is not better.**
Shallower trees, fewer verification passes, and higher cache-hit rates all improve cost-per-run
and are precisely the degradations this design exists to prevent. The efficiency metric is
trivially gameable by the thing being measured.

**Rule: no efficiency metric without a quality denominator.** Cost per run is meaningless. *Cost
per verified claim* and *cost per unit of stability achieved* are meaningful. Every ratio tracked
for optimization must be quality-normalized, or the loop converges on confident cheap garbage.

### What to track

Per node — tokens in/out split into **halo** (context replicated in) versus generated. This makes
P1's surface-to-volume ratio *directly observable* rather than a judgement call. Plus cost,
latency, model, cache hit/miss, verifier outcome, retries, and — for leaves — which base-case
condition fired.

Per subtree — realized branching factor `m` against the probe's prediction (the estimator's own
error signal, §4); planner predicted weights against actuals (weight calibration, §2); reduce cost
as a fraction of subtree cost, which is the empirical test of the §3 reserve guess.

Per run — cost per verified claim; cost per stable claim; stability rate; cap utilization and
**which cap bound**, money or time, which tells you across the population which constraint is
actually binding; degradation events.

Per line of inquiry — does refine reduce instability per dollar? That is the test of whether §8.1
works at all.

### The unit of learning is the problem class, not the run

The useful generalization is *"for problems shaped like X, strategy Y at depth Z with verification
ratio R yields stable results at cost C."* That requires keying on problem shape — the same key
§4's calibration corpus needs. One telemetry store, serving cost estimation and strategy selection
both.

### Guardrails

- **Learned priors are part of the reproducibility envelope.** If the planner's behaviour depends
  on a prior, and the prior drifts, the run is not reproducible. Priors must be **versioned and
  pinned in the run record** (P8), or replay silently breaks. This is easy to miss and expensive
  to retrofit.
- Priors are advisory and **visible at the plan gate**, never silently binding.
- No learned prior may reduce verification below the P3 floor. Verification density is not
  available for optimization.
- Telemetry aggregates over **shapes and metrics, never content**. Cache keys carry scope (P6);
  the telemetry store needs the same discipline or cross-tenant learning leaks scoped material.

---

## 9. Front end

Not a chat log. Conversation left, live tree right.

The OTel span tree *is* the decomposition tree — one span per node, a child node's span parented to
its parent node's span. Node states: planning / spending / verified / pruned, with a spend bar.
**Clicking a node shows its prompt, output, verifier result and cost** — the single affordance that
makes the system debuggable rather than a slot machine.

### Correction: one tree, two surfaces, not one event stream

This section originally claimed "the same event stream feeds Jaeger for developers and the SPA for
researchers." **That conflation is false**, and it was falsified by reading the agate that exists
rather than the agate this document assumed:

- **agate's SPA consumes a flat, ordered event union** (`route`, `model`, `answer`, `receipt`,
  `artifact`, …), not a span tree. It has **no node or plan event**, so the decomposition's *shape*
  has nowhere to go in it.
- **agate authors no OTel at all.** Tracing is agenkit's domain, delegated at runtime to AWS
  AgentCore Observability. agate's own boundary is drawn at exactly the point where OTel begins.

So there are **two projections of one run record**, deliberately, and neither is the other's
transport:

| surface | consumer | carries | omits |
|---|---|---|---|
| OTel span tree | Jaeger, AgentCore, CloudWatch | the **shape**, per-node cost, model version, verdict | prose, receipt formatting |
| flat `RunEvent` stream (NDJSON) | agate SPA | cost receipt, answer, trust summary | **the shape** |

Both are **lossy views of the `RunRecord`**, which remains the citable artifact (P8). A trace
carries wall-clock timestamps and random span IDs, so it is not byte-reproducible and must never be
treated as the artifact; the event stream flattens the tree away entirely. Nothing in quarry may
read a decision back out of either one.

**The consequence for this section was, for a while, that the live tree view described above had no
home.** A quarry run's *cost* and *trust* render in the agate SPA; its *shape* does not. The tree
view does not belong in a quarry-specific event type bolted into agate — asking agate for a
`node`/`plan` event would push quarry's model of computation into a layer that has correctly scoped
itself out of it. That remains a **standing divergence** about *agate*, and it is not resolved.

**Resolved differently: quarry grew its own surface.** The conclusion drawn from the divergence — "so
there is no viewer" — did not follow. It assumed the only candidates were agate's SPA and
agenkit/AgentCore, both of which quarry does not own. There was a third, which quarry does own: its
own command line.

| surface | consumer | carries | omits |
|---|---|---|---|
| **`quarry run` live tree (TUI)** | **a researcher watching one run** | **the shape, live, with per-node spend and verdict** | **nothing quarry knows; it is the record's own view** |
| OTel span tree | Jaeger, AgentCore, CloudWatch | the shape, per-node cost, model version, verdict | prose, receipt formatting |
| flat `RunEvent` stream (NDJSON) | agate SPA | cost receipt, answer, trust summary | the shape |

`cmd/quarry` is a **third projection**, and the only one that is quarry's to change:

- `quarry run` renders the tree as it happens, wired at the `Observer` seam. It is an *observer* in
  the strict sense — the record's bytes are identical whether or not anyone is watching (P8), which
  is asserted by running the same problem with and without `--quiet` and comparing hashes.
- `quarry show` is the click-through affordance §9 opens by naming: per-node prompt, output, verdict,
  cost, claims, gaps. Not live, but it is the "makes the system debuggable rather than a slot
  machine" requirement, and it works on any record on disk, including one someone emailed you.
- `quarry replay` proves the record reproduces (P8) — which no external viewer would have offered.
- `quarry plan` is the first half of the approval gate (§2): it produces the pinned artifact a host
  or a person approves, and `quarry run --plan` executes it. It is a *fourth* verb rather than a flag
  on `run` because the two phases have different exit conditions — planning that ends in a decline is
  a success, and a run refused for lack of authority is a usage error — and because a phase whose
  entire purpose is to stop before spending should not share an entry point with the one that spends.
- `--fake` runs the whole thing with no credentials, no network and no money, so the surface is
  demonstrable before any of the three integrations exist.

The lesson generalizes and is worth stating because this document got it wrong once: a divergence
about *someone else's* protocol constrains what quarry can send them. It says nothing about what
quarry can render itself.

**A divergence reporter that cannot name the divergence is worse than none.** `quarry replay`
compares canonical bytes and then calls `diffRecords` to say *which guarantee broke*, since "byte 4192
differs" helps nobody. But `diffRecords` compared only `Content` and `Cost`, so the 22 differing
`BaseCase` fields above fell through to its last-resort message: *"the canonical bytes differ but no
field-level difference was found — likely a field ordering or encoding change, which is itself a P8
break."* That sentence **blames the encoder for a difference the function simply did not look for**,
and it sent the investigation to the wrong file. It now compares every field `Canonical()` hashes,
and the fallback is reserved for the case it actually describes. Two details are load-bearing:

- `Verified` is a `*bool` because nil means *not checked*, which is not checked-and-failed (§8).
  Comparing the pointers would miss a nil-vs-false flip — the one difference that turns "we did not
  look" into "we looked and it failed".
- A `Gap` flip is named as a category confusion in those words, because that is the failure the
  unfunded sentinel exists to prevent (§7 above), and the reporting layer should say so rather than
  make a reader infer it from a diff.

This is also where the first tests in `cmd/quarry` came from. Four of the six defects in this round
lived in this package — the depth-bound derivation, this reporter *twice*, and the replayability
guard — and none was reachable from the library's tests, because **the CLI is where a record is turned
back into an executor**, and nothing else does that.

Fixing this reporter is also what *surfaced* the floor defect (§7): the floor had been diverging
silently the whole time, reported as an encoding break. **A blind spot in a checker hides defects in
proportion to how much you trust the checker**, and the two derivations it was hiding were in the same
file as itself.

**And it fell through a third time, on the one case a citable artifact exists to detect.** The
reporter skipped `Claims` on the stated grounds that they were covered by `Content`. They are not, and
the asymmetry is the whole reason: **content is replayed from the record; claims are re-extracted from
it.** A replay hands back the recorded content verbatim — that is what makes it a replay — and then
runs the extractor over it. So an *edited* record diverges in `Claims` and nowhere else: the tampered
content reproduces faithfully, and what disagrees is the claim set the record asserts versus the one
that content actually supports. A reporter blind to `Claims` blames the encoder for tampering.

This one was found by **scripting the demo** (`demo.sh`), not by a test and not by a sweep. The tamper
beat is the single demonstration where editing a record is the point, so it was the only thing that
had ever driven this path. Worth naming as a method: a demo is an adversary the test suite is not,
because it must exercise the claims in the order a reader would challenge them, and it cannot skip the
ones that are awkward to set up.

Two of those three defects were in code that could not be tested at all: `replayCmd` built its
executor from an inline struct literal and checked replayability with an inline condition, so a
missing field or an over-strict clause was reachable only by running the binary and reading a hash.
Both are now functions — `replayExecutor` and `replayableRecord` — for that reason alone. **The
wiring was what went wrong, so the wiring is what the tests assert on**, field by field; when
`RunBounds` gains a knob the test fails until it is carried across.

**Resolved since: `NodeOutcome` now records per-node timing and token counts.** This section
previously said "quarry has no per-node timing… a real live tree needs `NodeOutcome` to record time
first, which is a core change." That change was made. `NodeOutcome.Timing` brackets each node and
`Executor.Clock` — a *function*, deliberately distinct from `Executor.Now`, which is one fixed
instant that must not move mid-run or apportionment would drift — supplies it. The exporter now sets
real span timestamps, so a trace reads as a flame graph.

The two fields have **opposite reproducibility properties**, and reconciling that is the whole design
content of the change:

| | Property | Consequence |
|---|---|---|
| Token counts | deterministic property of a call | hashed into the record; a replay that dropped them would re-derive a different hash and read as a divergence that never happened |
| Wall-clock | differs on every run | **excluded from the hash** (`json:"-"`, and `canonical()` is JSON-based) — otherwise every replay would falsely report divergence |

So a record proves what was spent and decided, never how long it took. The trade is deliberate:
timing is observable but not citable. An unmeasured node reports *not measured* rather than zero
(`Duration()` returns `ok=false` on an unset, half-stamped, or reversed bracket, and the span carries
`quarry.timing.measured=false`) — the same absence-not-zero discipline as the three-state verdict,
because a fabricated sub-millisecond latency is exactly the kind of number people build dashboards
on.

Timing does *not* make the live tree exist; streaming remains unsolved for the reason given below.
What it removes is the excuse that the data was not there.

Two interactions matter more than the rest:

1. **Human-in-loop at the plan, before fanout.** Show the decomposition as an editable list and
   gate on approval. Per P3 this is the cheapest possible place to intervene. Under P9 the gate
   shows three things, not one: the split, **where the money goes**, and **what the cap excludes**.
   The researcher's real decision is usually "raise the cap or accept the reduced scope" — which
   they can only make if the exclusions are stated before spend.
2. **Live burn-down**, with mid-run top-up and per-branch kill. The cap becomes a dial the
   researcher holds rather than a wall they hit.

Partial results surface as they land. The Reducer streams last; nobody should watch a spinner for
four minutes.

**The two exported projections still do not stream**, and both remain folds of a *completed*
`RunRecord`. That is a consequence of where the telemetry seam sits: a sink is called when a node
**completes**, and children complete before parents, so a live *span* emitter would have to hand span
contexts down through the executor's recursion — which means the core importing an OTel SDK, and the
core imports no such thing by rule.

**The second seam this paragraph asked for was built: `Observer`.** It fires on node *entry*
(`Observer.Enter`, taking a `NodeEnter`) as well as on completion, which is exactly the "second seam
that fires on node entry" named above as the prerequisite. It carries no OTel and no SDK — it is a
two-method interface over plain structs, so the no-network rule is intact — and the TUI is its first
consumer. So the spinner problem is solved for the surface quarry owns and unsolved for the two it
exports to.

Three properties of that seam are load-bearing and were each a defect first:

- **Enter must fire before any work, including the plan.** A node that appears only once it has
  finished planning is invisible for the whole interval a watcher most needs to see.
- **`NodeEnter` carries the node's index among its siblings**, because children are entered
  *concurrently*: a renderer that inferred order from arrival order drew the tree in a different
  order on every run, for a tree whose shape is deterministic.
- **An observer runs on executor goroutines**, so it must not block and must not hold a lock across
  I/O. `tui.Tree` keeps two mutexes for this — one for the tree state, one for the output stream, and
  no path takes both.

### The trust summary can only be honest by being absent

The `RunEvent` stream carries a trust summary — the one thing a cost receipt cannot express, and
quarry's reason to exist at the front end. Its `stability` field is a **non-nullable number**, and
the object it rides on **forbids unrecognized keys**, so quarry cannot say "not measured" and cannot
say "this is a floor, compared by string equality" either. The whole vocabulary §7 built for
qualifying a stability estimate has no wire representation.

That leaves exactly one honest move: when the estimate is unpublishable (§7's three cases), **omit
the entire summary**. Absence reads as "this producer did not assess", which is true. A `0.0` would
read as "measured, and nothing replicated", which is a finding nobody made.

The cost is real and worth stating rather than absorbing: omitting the summary also drops the
verified/unverified counts, which were never uncertain. That is what a non-nullable field costs, and
it is why the qualifiers are recorded on quarry's own summary type even though none of them can
travel — a caller writing its own artifact should not have to re-derive them, and the day the field
becomes nullable the data is already there.

**The rule binds the caller, not the producer**, which is the part most likely to rot: the summary
type can compute the flag perfectly and the misleading zero still reaches the front end if someone
passes the object anyway. So it is asserted end-to-end on the emitted bytes, not on the struct.

### A fourth consumer: a supervising host, which is not a viewer

The table above enumerated *viewers* — things a person looks at. It missed a consumer that reads a
run without anyone watching: **another program that spawns quarry as a subprocess and decides what to
do next.** Two exist (bucktooth in Go, rustynail in Rust), and the requirement they brought is not a
rendering requirement at all.

| surface | consumer | carries | omits |
|---|---|---|---|
| **`quarry run --events-json`** | **a supervising host, choosing the next move with no human reading** | **the `RunEvent` stream plus a version and a terminal outcome; gaps and unfunded, separately; spend and cap as integers** | **the shape** |

It is the `RunEvent` stream **framed**, not a fourth protocol: a version line in front so a host can
*refuse* a stream, and an outcome line behind so it can *trust* one it read to EOF. Everything between
is byte-identical to what agate receives, asserted rather than assumed — three protocols kept in
lockstep by hand is the failure mode this avoids.

Three things this consumer needs that a viewer does not, and each is why a field exists:

- **A terminal marker.** NDJSON yields whole lines whether or not the producer finished, so a run
  killed after the artifact event is indistinguishable from one that completed. Only the *absence* of
  a terminal event says "crashed" in band — and a host reading a vendored fixture from a file has no
  exit code to fall back on.
- **A status vocabulary, not a boolean.** "Finished", "ran out of time", "ran out of money" and
  "crashed" call for four different next moves, and a host choosing automatically will offer a
  deadline raise where money was needed unless the status tells them apart. That is the §3.1
  mislabelling `ErrRecordedUnfunded` prevents, one layer out — which is also why **cap-bound
  degradation exits 0**: it is planned degradation inside authority, and a non-zero status would make
  P4's contract look like a malfunction every time it worked.
- **Money as integers.** Everything in agate's union prices in USD floats because agate does, and a
  real 25-node receipt's rows do not sum to its total in float64. The terminal event is quarry's own,
  so it carries the ledger's `int64` micro-units and there is nothing to reconcile.

**This does not resolve the standing §9 divergence, and it is worth being clear why.** agate still has
no gap representation, so the one fact a supervising host most needs — did this answer cover the
question, or part of it — cannot ride on any event agate accepts. quarry does not widen agate's
contract to fix that; it frames its own events around agate's, namespaced `quarry_*`, and agate's
`build_artifact` skips both frames. The divergence is *routed around*, not closed.

The contract is written down in `docs/integration-requirements.md` §6 rather than here, because it is
an integration surface and that is where the ones quarry must hold in lockstep with another repo live
— even though this is the only one quarry owns outright. The vendorable fixtures are
`testdata/runevents/`.

### The host stream now streams, and the fold was not the thing to widen

"**The two exported projections still do not stream**", above, is now half wrong, and the half that
changed is the host stream. `quarry run --events-json --live-events` emits a per-node event at *entry*
and at *completion*, as they happen, ahead of the fold. The OTel projection still does not stream, for
the unchanged reason stated above: a live span emitter needs span contexts threaded down the
recursion, which means the core importing an SDK.

The paragraph's diagnosis was right and its implied remedy was wrong. It located the obstacle in the
*telemetry seam* firing on completion — true — and the fix was not to widen that seam or the fold, but
to project the `Observer` seam that was built for the TUI onto a wire. The fold stays a fold of a
completed record. Nothing about it changed.

Four decisions, because each had a wrong answer that looked reasonable:

- **Whose protocol.** quarry's own. The alternative was asking agate for a `node` or `plan` event, and
  that would push quarry's model of computation into a layer that has correctly scoped itself out of
  it. The §9 divergence stays routed around, exactly as the framing above routes around the missing
  gap representation — not closed in the wrong layer.
- **Where it goes.** The **same stdout**, interleaved, as additive kinds under stream version 1 — no
  bump, because the frame's own frozen rules say adding a kind is minor and a consumer must not key on
  line position. A second destination was the alternative and is worse: it forces a host to correlate
  two streams with no ordering guarantee between them, and *the ordering is the whole value* — a
  node's live entry must be readable as preceding the fold that summarises it.

  One consequence: **the version frame moves to run start.** A host must be able to refuse a stream
  before it reads anything it would parse, and a frame written after the first live event arrived came
  too late. So exactly one frame per stream, and the fold gained a variant that omits it. The live
  kinds carry a **separate version**, per event rather than in the frame — different consumers with
  different tolerances, and a host may attach mid-stream where the frame has already gone past.
- **What a live event may claim: nothing not yet true.** Made structural rather than promised. The
  entry event carries *only* fields known at entry — there is no verdict field to leave nil and no cost
  field to leave zero, so a dashboard cannot render an in-flight zero as a measured one. On the exit
  event the three-state fields survive to the wire as values no measurement produces:
  `verdict` is a string enum whose third state is `not_assessed` (and unchecked is the *common* case
  under P2, not the exception), `duration_micros: -1` is unmeasured — **not 0**, which is a plausible
  sub-millisecond duration — and `alloc_micros: -1` is Unlimited. This is the fabricated
  `stability: 0.0` defect, refused at a new site before it shipped.
- **Only TIME produces a gap**, on this wire too. `gap` and `unfunded` are separate keys, always
  emitted, never both true of one node. A live view that painted a priced-out node red would make P4's
  contract look like a malfunction while it worked.

**An observer must not perturb the run it observes**, which the seam's own doc already required and
the wire makes testable: the record's bytes are identical whether or not anyone is watching, asserted
on the emitted record. A live write failure is therefore *recorded and the run continues* — failing a
run because a viewer's pipe closed would let an observer kill the thing it observes — and the
truncated stream is itself the honest signal, since a host finds no terminal outcome and reports a
crash.

None of it is citable. The `artifact` event's url names the record; that is the artifact.

---

## 10. Execution and state

A live orchestrator process for the duration of a run (P5). Simple, and it dies with the run.

**Checkpoint the tree to durable storage as it expands anyway** — not for NO CLOCKS reasons but
because runs are long and expensive, and a process that dies at minute eighteen of a twenty-minute
run has burned real allocation for nothing. The process is the optimization; the durable state is
the correctness story. Resume costs only unfinished subtrees, since the cache already records what
completed.

**Cancellation must propagate.** When the Reducer prunes a branch, the subtree dies. Natural
in-process; needs explicit design if the executor ever becomes a separate service.

---

## 11. Build order

1. Ledger arithmetic + admission control, in-process. Fake provider. **Caps work end to end.**
   Node-level telemetry (§8.2) records from here on — it is nearly free and the corpus only
   accumulates with time.
2. Planner / Parallel / Reducer with a fixed depth of 1. No recursion yet.
3. Content-addressed cache with scope in the key. Recursion on. DAG behaviour.
4. Verifier interface + mechanical oracles. P2 becomes a real termination condition.
5. Run record + replay. Determinism test: replay twice, compare bitwise.
6. Claim extraction and equivalence (§7) — the risky piece; start spiking it in parallel from day 1.
7. Replicates, stability reporting, plan pinning.
8. Probe + Galton-Watson estimator; calibration corpus accumulates from step 1 onward.
9. Adversarial passes and surplus-budget policy.
10. Integration: chokepoint, scope tags, agate stack. (Not "SPA tree view" — see §9: the tree view is
    quarry's own TUI, and it needs nothing from agate.)

Steps 1–7 have no AWS dependency and no LLM dependency beyond a provider interface.

---

## 12. Open questions

- **Naming.** `quarry` is a placeholder.
- **Normalization for the cache key.** What makes two sub-problem statements "the same"? Embedding
  threshold, canonical rewrite, or exact-match-after-templating? Affects hit rate and correctness in
  opposite directions. Currently exact-match, **scope-qualified** — which under-hits deliberately,
  because a missed hit costs money and a false hit corrupts an answer and can leak across a scope
  boundary (P6). Any move toward similarity matching has to keep that asymmetry.
- **Claim extraction format.** This read "**Still the blocker for §7**." It is no longer the blocker:
  a `Claim` carries its **pinned normalized form**, so equivalence replays under the exact
  normalization that produced it (P8) and a downstream comparator in another language matches with a
  bare string compare — and the semantic comparator now sits behind `ClaimComparator` (§7). Three
  findings from building it are properties of the *format's consumers*, so they belong here rather
  than only in §7: comparison is **three-state** (unassessable is neither agreement nor
  disagreement, and must be counted separately or it launders silence into a finding);
  comparison is **metered** (it is the first post-hoc analysis in the system that spends money, so it
  is sized by an `Estimate` and **admitted before the call**, and a paid comparator with no ledger is
  *refused*, not quietly run — the first implementation debited afterwards and a live run spent 200
  micro-units against a 1-micro-unit cap while honestly reporting truncation, which is why the seam
  carries `Estimate` rather than leaving sizing to the caller); and
  clustering must be **order-independent**, which a non-transitive comparator makes a real constraint
  rather than a stylistic one. Source spans remain unpopulated and undesigned — they need retrieval,
  which does not exist yet. The interface is deliberately stateless so the eventual move behind a
  service boundary stays cheap.
- **Apportionment policy.** v1 is proportional to planner relative weights. The MCTS-style
  marginal-value version is v2 and needs a value model that does not yet exist.
- **Reserve fraction.** 60–70% apportioned is a guess. Too high and nodes cannot retry or reduce;
  too low and the tree is starved and shallow. Should be measured, and may need to vary with
  depth — deep nodes have less need for reserve since they have fewer children to fund.
- **Whether the planner can be trusted to fit at all.** Relative weights are more reliable than
  absolute, but a planner that systematically under-weights hard children will produce plans that
  fit on paper and overrun in practice. Mitigation is the mechanical check plus actuals feeding
  back into weight priors, but this is unvalidated.
- ~~**Unit of account.**~~ **Settled: micro-dollars, as `int64`.** Forced rather than chosen — agate
  rounds USD to six decimal places, which is micro-dollars exactly, so the two systems agree to the
  unit with no float conversion anywhere. `Units` is integral so apportionment (largest-remainder)
  sums exactly and replay is bit-stable (P8). Tokens remain a *measurement* on `Sample`, not a unit
  of account; a campus allocation unit, if it ever exists, converts at the edge like USD does.
- **Does the planner ever see sibling results?** Strict independence is simpler and is assumed
  here; a re-planning step after partial returns is more powerful and much harder to account for.
- **Failure semantics of a partially-completed tree.** Largely settled by §3.1 — a deadline leaves
  no option but to return what exists, so *degraded answer with gaps marked* it is. The
  silent-vs-named half is now **decided: named, always.** An unreturnable node is a gap, and a gap
  is disclosed. The sharper ruling the implementation forced is that **only time produces a gap** —
  budget exhaustion is *planned degradation*, a decision the system made inside its authority, and
  labelling that a gap would make the cap look like a malfunction. Applying that ruling at a
  *reducing* node needs two flags where one is tempting: "the reducer's input was incomplete" and
  "this node was cut short by time" are different facts, and one variable serving both made every
  priced-out subtree report a gap — so an unaffordable run was indistinguishable from a deadline
  miss, which is the distinction the receipt exists to draw. A child's gap does propagate, since a
  parent whose child was killed by the deadline is itself time-truncated. What remains open is the
  original hard part: how the reducer signals confidence when merging over a tree it knows is
  incomplete.
- **Anchoring on refine (§8.1).** How much of a prior run to show the planner without collapsing
  the new plan onto the old one. Findings-only, findings-plus-shape, or paired sampling with and
  without. Interacts badly with §7 — the strongest replication signal is an independent
  decomposition, and iteration is the thing most likely to destroy it. **Now narrowed rather than
  settled:** the implementation ships findings-only, withholding the tree shape, on the reasoning
  that a biased plan is undetectable in the record while a missing signal is merely absent. That is
  a defensible default, not a validated answer — what is still open is the paired-sampling
  experiment that would show whether the anchored arm actually plans better, and it cannot run until
  there are enough real refines to compare.
- **When learned priors stop being advisory (§8.2).** Measurement and learning are safe;
  optimization is where the Goodhart risk lives. There is no principled threshold yet for how much
  evidence justifies letting a prior actually steer the planner, nor a decided policy on whether
  a researcher can opt a run out of learned priors entirely for replication purposes.
- **Pricing the slack.** If a due date buys a batch discount, the interface implies a quoted price
  that varies with the deadline. Whether to expose that as an actual quote or merely as guidance
  is undecided, and the former has obvious ways to be wrong.
**Two questions raised here have since been settled — recorded because the reasoning is the design,
not the answer.** Both were about `NodeOutcome`: whether it should carry per-node timing (raised by
§9) and whether it should carry token counts (raised by §8.2). It carries both.

- **Timing.** The concern was that a duration is the one field that **cannot be reproduced**, so it
  either sits in the record and breaks byte-identical replay (P8), or sits outside it and is absent
  from the artifact everything else joins to. The resolution takes the third option the framing
  missed: the field sits *on* the outcome but *outside* the hash. `canonical()` is JSON-based, so
  `json:"-"` is sufficient and mechanical. Yes, "the record has a field replay must ignore" erodes
  P8's simplicity — the erosion is paid for by a test that runs one tree under two different clocks
  and asserts identical bytes, with a non-vacuity guard proving the durations really differed. If a
  future change hashes `Timing`, that test fails, and the failure is the design speaking.
- **Tokens.** They lived only on `Sample`, so a sink saw cost but could not compute surface-to-volume
  — the single number that makes P1 observable was the one the observer could not reach. Now on the
  outcome, and the aggregate `Metrics.SurfaceToVolume()` exists. It survives the §8.2 Goodhart
  guardrail *because* both its terms are work done: unlike cost-per-run, it cannot be improved by
  verifying less, recursing shallower, or taking more cache hits, and it is never divided into spend.
  Two smaller calls fell out of wiring it — an internal node reports the **reduce call's own** tokens
  rather than a subtree roll-up (rolling up would double-count in every ancestor), and a cache hit
  reports real tokens with zero cost (the tokens were genuinely spent once; *this* run paid nothing).
  The original suspicion that §8.2's metrics and §8's record "want slightly different shapes" was
  right, and the shape they wanted was one struct with a hashed half and an unhashed half.

---

## 13. Honesty note

**This section originally read "Nothing above is implemented." That is no longer true, and the
honest replacement is more specific than a status line.** The Go implementation has built steps 1–10
of §11: ledger and apportionment, planner/fanout/reducer, cache and DAG collapse, verifiers,
byte-reproducible records and replay, claim extraction, replicates and stability, the estimator, the
adversarial surplus pass, and the integration seams. The determinism test passes: replay twice, bytes
identical.

What remains weak is not the same as what remains unbuilt, so the two are separated:

**Built since the list below was written.** The planner and reducer are model calls
(`provider.BedrockPlanner`, `provider.BedrockReducer`) rather than the fixtures every earlier step
ran on — quarry's central act is finally performed by a model. Portfolio is a first-class strategy
(§2). Both landings behaved the same way, and it is the pattern this section exists to name: **each
one exposed machinery that had been silently correct only because nothing exercised the other case.**
Portfolio found three — same-key allocation collapse (a defect for partitions too), same-key dedupe,
and arms served each other's cached answers. The model-backed planner and reducer found that the
determinism test had stopped covering the two nodes it most needed to. None was a new bug; all four
were pre-existing, and all four were invisible until a second case arrived.

**Both agents are now verified against a real model, not only against a fake.** They had been tested
entirely through a recording double written by the same author as the prompts — which can confirm that
the parsing code reads what it expects and can say nothing about whether a model produces it. Live
tests (`provider/live_agents_test.go`, gated on `QUARRY_LIVE`) confirm the three contracts the code
actually depends on: the planner returns parseable JSON with positive relative weights; it **does**
decline an atomic question rather than splitting it, so P1 is operative rather than decorative; and the
selector returns a bare index, so an arm comes back **verbatim** rather than rewritten — the one
result no fake could establish, since selection-becomes-generation is exactly what a compliant double
cannot exhibit. The reducer also hedged unprompted-by-the-test on partial input (§3.1). What these
tests deliberately do **not** assert is the quality of the judgement: whether a split was a *good*
split is not a property a test can hold a model to, and one that tried would fail on prompt drift and
teach nothing.

The comparator (§7) is gated the same way (`provider/live_comparator_test.go`), and the one assertion
that only a live model can settle is the whole justification for the feature: that a genuine paraphrase
the free rung cannot judge comes back **SAME**, while an opposite conclusion about the same topic —
"prices rose" against "prices fell", lexically close and the error a similarity measure makes — comes
back **DIFFERENT**. A fake returns whatever its fixture says, so it can establish neither. Both hold
against a real model, and the paraphrase clusters into a 2-of-3 majority the free rung reports as three
separate claims — the undercount closed, measured rather than argued.

**And the live run immediately found a P4 violation in the code it was testing.** The cap test printed
`cost=0.0002 truncated=true` against a **1-micro-unit** cap: 200× over, disclosed rather than
prevented. This is the strongest instance yet of the pattern this section exists to name, because the
gap was not between the doc and the code — `ClusterClaims`' own comment had described admit-first
behaviour for as long as the function existed, so *reading* it would have confirmed a guarantee that
was not being met. Two things were wrong at once: the interface had no way to size a comparison, and
the test asserted the disclosure rather than the spend. A unit test with a fake comparator could have
caught it and did not, because the fake's cost was whatever the fixture said and the assertion never
looked at the ledger.

**And fixing it exposed a second leak one function further downstream.** Everything the comparator
seam was careful about — the abstention verdict, the separate unassessed count — was being flattened
to a bare `0.0` by the projection that hands a trust summary to a front end, where it reads as
*measured, and nothing replicated* (§7, §9). Two replicates asserting one conclusion in different
words produce exactly that. No test failed, because every test looked at a layer that was correct.
The generalizable part: **a distinction is only preserved if the last hop preserves it**, and the
last hop is usually the one nobody thinks of as making a claim. This one was found by *reading* the
projection against the newly-added report fields rather than by running anything — the counterpart
to the entry above, and the cheaper of the two.

The follow-on makes the same point about *this document*: plan pinning's missing weights (§7) had been
recorded here as a limitation for as long as pinning existed, and were fixed only after the identical
failure — a pinned re-run silently varying something it claimed to hold fixed — turned up a second
time in `Strategy`. A limitation written down is not a limitation understood. What made it act was
noticing it was an instance of a pattern, not that it was listed.

**Built, and known to be weaker than it looks.**

- **§4 cost estimation** is implemented and remains the weakest section — it depends on a calibration
  corpus that does not exist until the system has run many times, so early estimates will be poor and
  are labelled advisory. Nothing gates on them (P4), which is the only reason shipping them is safe.
- **§7 claim equivalence.** This read: "*is the highest technical risk and is currently
  **normalized-string equality, not semantic**. Agreement phrased differently is missed, so stability
  estimates **undercount** — the safe direction, but the number is a floor, not a measurement.*" The
  semantic rung now exists (`ClaimComparator`, `provider.BedrockComparator`), so the floor can be
  raised to a measurement **when a paid comparator is wired and a ledger is supplied**. What has not
  changed is the default: with no paid rung, stability is still a floor, and the report now says which
  comparator produced the number (`ComparedBy`) precisely so a floor cannot be mistaken for a
  measurement. Two things landed with it that were not anticipated here. First, the seam had to widen
  rather than deepen — a bool predicate cannot carry a deadline, a cost, or *I could not tell* — which
  is the third time in this project that a signature turned out to be the design decision. Second,
  and the pattern §13 exists to name: **probing the existing clustering before writing the comparator
  found a live defect that no test could have caught.** Single-link clustering was order-dependent
  under any non-transitive relation, and string equality is transitive, so the bug was invisible until
  the moment the comparator upgraded — it would have activated *with* the feature meant to improve
  accuracy, and in the direction that reports unanimous agreement no comparator asserted. That is now
  the fourth instance of "silently correct only because nothing exercised the other case," and the
  first one caught **before** shipping the second case rather than by it.
- **§5's verification ladder** is well understood in the literature, but its *cost calibration* for
  this workload is guesswork until measured. Verifier availability, not depth, is the real
  terminator (P2), and that makes the ladder's economics load-bearing rather than incidental.

**Falsified by contact with the other systems, and corrected in place.**

- **§9's premise was wrong** — one event stream cannot feed both Jaeger and the agate SPA, because
  agate's protocol is flat and has no representation for a tree. Corrected above. This is the clearest
  case in the document of an assumption that survived only until someone read the neighbouring repo.
- **§9's *correction* was then wrong too**, in a more interesting way. Having established that agate
  cannot carry a tree, it concluded the live tree view "has no home today" — surveying only the
  viewers quarry does not own. `cmd/quarry` is quarry's, and building it took no new protocol from
  anyone. Recorded here rather than quietly amended because the failure mode is the one this section
  exists to catch: a real constraint, correctly identified, over-generalized into a blocker.
- Several §1-level integration assumptions were also wrong in detail (an overloaded HTTP status
  treated as a clean cap signal; a receipt row per leaf when internal reduce nodes also spend). Both
  are recorded in `docs/integration-requirements.md` rather than quietly fixed.

**Not built, and honestly blocked rather than deferred.**

- **A streaming *export*.** The OTel span tree and the `RunEvent` stream are still post-hoc folds of a
  finished record, for the reason §9 gives: live span parentage would require the core to import an
  OTel SDK. Genuinely blocked on a design decision, not a missing capability.
- **Mid-run human-in-loop.** Approving *before* the run is built (§2, `quarry plan` / `run --plan`);
  interrupting a run in flight to approve or amend a sub-plan is not, and is a different problem — it
  needs input handling and a way to pause a subtree without cancelling it.
- **Mid-run top-up and per-branch kill** (§9's second interaction). The mechanism exists —
  `context.CancelFunc` per branch (§10) — and the display exists; the input handling does not.

Four items previously listed here — per-node timing, per-node token counts, **the live tree view**,
and **human-in-loop at the plan gate** — **were built**; see the settled questions in §12 and the
corrections in §2 and §9. They are called out because they show the failure mode this section exists
to catch: each was described as blocked when none was, and the actual obstacle in every case was an
unmade decision, not a missing capability. The tree view is the sharpest instance, because its
blocker was stated as "a viewer that exists" while the answer was to write one.

The plan gate is the sharpest instance of the *other* half, because its blocker was stated accurately
and was still not a blocker. The entry read: "it needs a decision about what a declined plan does to a
run record, which is a question about the artifact rather than about the interface." That was true —
and the answer, once someone made it, was one sentence long: a declined plan is a valid artifact that
runs as a single node (§2). **An unmade decision described precisely enough to sound like a
dependency is the form this failure takes when it is hardest to see**, because the sentence is
correct, informative, and still an excuse.

**The fake provider is not a small live provider, and three defects hid in the difference.** The
first run against real Bedrock — a 28-node tree under a $0.25 cap — produced three divergences in
`quarry replay`, all recorded above (unfunded-is-not-gapped in §7, the depth bound in §7, the
divergence reporter in §9). The whole suite was green, and every `--fake` record had replayed
byte-identically for as long as `--fake` had existed.

Fixing the third then exposed **two more** — the floor (§7) and the replayability guard (§7) — both
found by a `--fake` sweep, and both of which `--fake` could have found at any point. And the reporter
itself turned out to have a **third** blind spot, `Claims` (§9), found later still by scripting the
demo. Six in total, and the split is the lesson: the live run was needed to find the first three, but
only because it repaired the *reporter* that had been hiding the rest. **A broken checker converts
every defect downstream of it into a false clean** — and the count is not a coincidence, since three
of the six were only ever visible *through* the one component that could not see them.

The reason is structural rather than a matter of scale, and it is the useful part:

| | `--fake` | live |
|---|---|---|
| Per-call cost | uniform | varies several-fold per sub-question |
| Consequence | affordability funds *all* children or declines the split | **some** children priced out — a shape `--fake` cannot reach |
| Depth | planner declines on clause length first | the depth bound is actually reached, so `max_depth` leaves exist |

So `--fake` could not construct either state the two derivations got wrong. This does not retract
"anything reachable with `--fake` must stay reachable with `--fake`" — that constraint is what makes
the system demonstrable, and it has earned its place by finding four defects of its own. What it adds
is the converse: **`--fake` bounds what the fixtures can construct, and a uniform cost model is the
specific way it is unrepresentative.** Both defects were reproducible in-core once known — a weighted
plan with a per-node estimate gets there without AWS — so the gap was in what anyone thought to build,
not in what was buildable.

The corollary for tests: the six defects lived in `record.go`, `types.go` and `cmd/quarry`, and
`cmd/quarry` had **no test file at all** until this run, because it is the only place a record is
turned back into an executor. A derivation that exists solely in a projection is tested by nothing the
library asserts.

One more thing the sweep is due credit for. Once the reporter could name a divergence, a nine-case
`--fake` sweep across caps, deadlines, depths, floors and scope tags became a usable regression check,
and it found both remaining defects in the cases at the extremes — the caps so small that the root
itself was priced out. **The interesting records are the degenerate ones**, which is the same thing
§3.1 says about partial runs and the same reason the all-gap record had to be replayable: a cap that
bites hard enough to leave a one-node tree is not a broken run, it is the clearest possible example of
the behaviour this system is built to record.

Three discovery routes, then, and none of them was the test suite: the live run (three defects), the
degenerate-case sweep (two), and **writing the demo** (one). The third is the cheapest and was the
last to be tried. A demo must exercise the claims in the order a sceptical reader would challenge
them, which is a different order from the one the code is organised in — and it cannot quietly omit
the awkward setup, which is exactly where the tamper path had never been driven. `demo.sh` is
maintained for that reason as much as for showing the system.

The pattern worth naming: every correction above came from *running or reading the real thing*, not
from re-reading this document. The sections still marked weak are precisely the ones that have not
yet met a real corpus — and, as the table above records, "running the real thing" now demonstrably
means the *live* thing at least once, not only the fake.

**The approval gate was verified live, and the live run added two things `--fake` structurally could
not.** One live plan plus one gated run at `--cap 0.25 --depth 2` (artifact `889e3f108f36`, record
`1dc68ab8d4bd`, 26 nodes, $0.0669): all five approved children ran verbatim in order, the record
carried the artifact's `PlanID`, and `quarry replay` reproduced it byte-identically with no
credentials. Every refusal — cap, widened scope, depth, tampered artifact — refused before reaching a
provider, so each cost nothing, which is the property that makes a gate a gate.

The two additions are both consequences of the uniform-cost gap above:

- **The exclusions path.** The live planner proposed nine sub-questions and the cap covered five, so
  the artifact carried four *excluded* ones and the record's summary reported them as planned
  degradation rather than gaps (§3.1). `--fake` declines or funds everything, so it never produces a
  partial split at all — the single most important thing a host is being asked to approve.
- **A measured planner call, which sizes issue #26.** `--plan-cap` is the first mechanism that ever
  priced one: **$0.0033**. Against the same run's six internal nodes that is ~$0.0198 unrecorded
  against a $0.0669 recorded total — the receipt under-reports by roughly **30%**. The mechanism was
  known and documented; the magnitude was not, and it is the magnitude that makes it a defect rather
  than a rounding note. It is invisible on `--fake` for a reason worth stating, because it is a second
  face of the same asymmetry: a live planner's prompt carries the balance, the depth, the prior
  outcomes and the shape rules, making it one of the *largest* prompts in the system, while a fake
  planner's call prices out near a leaf's. The ratio only exists live.
