# ADR 0001: Fixed three-node Raft-lite consensus

## Status

Accepted

## Context

QuorumLimiter must make rate-limiting decisions that stay consistent across
multiple independent servers. A naive in-memory limiter running on N servers
lets each server allow the full quota, so the effective limit is multiplied by
N. We need every state-changing decision to be agreed upon by a majority before
it takes effect.

We also want the project to demonstrate real distributed-systems mechanics
(leader election, replicated logs, quorum commit, recovery) rather than hiding
them behind a library.

## Decision

Implement a from-scratch "Raft-lite" consensus layer with **fixed three-node
membership**. Three nodes tolerate one unavailable node while keeping a majority
(quorum = 2). Membership is static: nodes are configured up front via
`QL_PEERS` and never change at runtime.

We deliberately exclude dynamic membership, joint consensus, snapshots, log
compaction, leadership transfer, pre-vote, and read-index. These are recorded as
future work.

## Consequences

- Simpler, understandable implementation focused on core Raft safety rules.
- One unavailable node is tolerated; loss of quorum correctly blocks writes.
- The log grows without compaction, so deployment must monitor size and back up.
- This is an **educational** implementation, not a substitute for a formally
  verified production consensus library. That boundary is stated honestly in the
  README and interviews.

## Alternatives considered

- **Import a Raft library** (etcd/raft, hashicorp/raft): rejected because the
  goal is to demonstrate and understand consensus, not consume it.
- **Redis or a managed limiter**: rejected; it removes the distributed-systems
  learning objective and adds an external dependency.
