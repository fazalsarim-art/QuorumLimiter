# ADR 0002: Deterministic integer token bucket

## Status

Accepted

## Context

Every node applies committed commands to its own copy of the state machine. If
applying the same command in the same order could produce different results on
different nodes, the replicas would diverge and consensus would be meaningless.

Token-bucket refill involves division (tokens added over elapsed time). Floating
point arithmetic can round differently across platforms and compilers, which is
unacceptable in a replicated state machine.

## Decision

Use a **deterministic token bucket implemented entirely in integer arithmetic**.

- Balances are stored in **milli-tokens** (1000 milli-tokens = 1 token).
- Refill uses integer division and **preserves the remainder** so no fractional
  credit is lost or invented:
  `numerator = elapsed_ms * refill_tokens * 1000 + refill_remainder`,
  `added = numerator / refill_interval_ms`,
  `new_remainder = numerator % refill_interval_ms`.
- Time comes from the **timestamp inside the committed command**, never a local
  clock during apply. Effective time is `max(command_time, last_refill_time)` so
  time cannot move backward after a leader change or clock correction.
- Multiplications are bounds-checked to reject overflow.

## Consequences

- All healthy nodes that apply the same command sequence reach byte-identical
  state, which is verifiable in tests.
- No floating point anywhere in the apply path.
- Policy updates must deterministically convert existing buckets at the update
  timestamp, which can be expensive for policies with many subjects; the
  dashboard warns and metrics measure apply duration.

## Alternatives considered

- **Floating-point tokens**: rejected due to cross-platform nondeterminism.
- **Recomputing from a local clock on each node**: rejected; nodes would diverge
  after leader changes or clock skew.
